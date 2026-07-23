package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type job struct {
	rel  string
	abs  string
	size int64
	mt   int64
}

type result struct {
	rel       string
	title     string
	size      int64
	mt        int64
	pageCount int
	meta      string
	category  string
	tags      []tagPair
	hasCover  bool
	err       error
}

// startWorkers launches the persistent worker pool. Workers do all the heavy
// lifting (zip parsing + thumbnailing) and never touch the database; their
// results flow back to the scanner, which is the single writer. That keeps
// SQLite write contention out of the picture entirely.
func (a *App) startWorkers() {
	for i := 0; i < a.cfg.Workers; i++ {
		go func() {
			for j := range a.jobs {
				a.results <- a.process(j)
			}
		}()
	}
}

func (a *App) process(j job) result {
	r := result{rel: j.rel, size: j.size, mt: j.mt, title: titleFromRel(j.rel)}
	arc, err := openArchive(j.abs)
	if err != nil {
		r.err = err
		return r
	}
	defer arc.Close()
	imgs := arc.Images()
	r.pageCount = len(imgs)
	r.meta = arc.Meta()
	r.category, r.tags = parseInfo(r.meta)
	if len(imgs) > 0 {
		if rc, err := arc.Open(imgs[0]); err == nil {
			if err := makeThumb(rc, a.cfg.ThumbWidth, a.coverPath(j.rel)); err == nil {
				r.hasCover = true
			}
			rc.Close()
		}
	}
	return r
}

func titleFromRel(rel string) string {
	base := filepath.Base(rel)
	name := strings.TrimSuffix(base, filepath.Ext(base))
	return strings.TrimSpace(strings.ReplaceAll(name, "_", " "))
}

func (a *App) coverPath(rel string) string {
	return filepath.Join(a.cfg.CoversDir, pathKey(rel)+".jpg")
}

// Scan walks the library, diffs it against the index, and processes only new or
// changed archives. It is safe to run repeatedly; removed files are pruned.
func (a *App) Scan() {
	if !a.scanning.CompareAndSwap(false, true) {
		return // a scan is already running
	}
	defer a.scanning.Store(false)
	start := time.Now()

	// 1. current state on disk, keyed by slash-normalized relative path
	type fsEntry struct {
		abs  string
		size int64
		mt   int64
	}
	disk := map[string]fsEntry{}
	filepath.WalkDir(a.cfg.RootDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if indexed, _ := archiveMode(d.Name()); !indexed {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(a.cfg.RootDir, p)
		if err != nil {
			return nil
		}
		disk[filepath.ToSlash(rel)] = fsEntry{abs: p, size: info.Size(), mt: info.ModTime().Unix()}
		return nil
	})

	// 2. known state in the index
	type dbEntry struct {
		size   int64
		mt     int64
		status string
	}
	known := map[string]dbEntry{}
	if rows, err := a.index.Query(`SELECT path, size, mtime, status FROM archives`); err == nil {
		for rows.Next() {
			var p, st string
			var sz, mt int64
			if rows.Scan(&p, &sz, &mt, &st) == nil {
				known[p] = dbEntry{size: sz, mt: mt, status: st}
			}
		}
		rows.Close()
	}

	// Safety guard: if the walk found nothing but the index is populated, the
	// library mount has almost certainly gone missing. Bail out rather than
	// prune every row and delete every cover (an expensive, pointless rebuild).
	// user.db is untouched regardless, so favorites/progress are never at risk.
	if len(disk) == 0 && len(known) > 0 {
		log.Printf("scan: 0 archives on disk but %d indexed — library mount looks missing; skipping this scan", len(known))
		return
	}

	// 3. anything new, changed, or not yet successfully indexed gets a job
	var todo []job
	for rel, fe := range disk {
		k, ok := known[rel]
		if !ok || k.size != fe.size || k.mt != fe.mt || k.status != "indexed" {
			todo = append(todo, job{rel: rel, abs: fe.abs, size: fe.size, mt: fe.mt})
		}
	}

	// 4. prune rows whose files are gone, and drop their covers + tags
	for rel := range known {
		if _, ok := disk[rel]; !ok {
			var id int64
			a.index.QueryRow(`SELECT id FROM archives WHERE path = ?`, rel).Scan(&id)
			a.index.Exec(`DELETE FROM tags WHERE archive_id = ?`, id)
			a.index.Exec(`DELETE FROM archives WHERE path = ?`, rel)
			os.Remove(a.coverPath(rel))
		}
	}

	log.Printf("scan: %d archives on disk, %d to (re)index", len(disk), len(todo))

	// 5. feed jobs to the pool and collect results here (single writer)
	go func() {
		for _, j := range todo {
			a.jobs <- j
		}
	}()
	for i := 0; i < len(todo); i++ {
		a.writeResult(<-a.results)
	}

	a.lastScan.Store(time.Now().Unix())

	var dups int
	a.index.QueryRow(`SELECT COUNT(*) FROM (
		SELECT identity FROM archives WHERE identity LIKE '#%' GROUP BY identity HAVING COUNT(*) > 1)`).Scan(&dups)
	if dups > 0 {
		log.Printf("scan: %d duplicate IDs detected — favorites on those are ambiguous; see /api/id-report", dups)
	}
	log.Printf("scan: done in %s", time.Since(start).Round(time.Millisecond))
}

func (a *App) writeResult(r result) {
	status, errStr := "indexed", ""
	if r.err != nil {
		status, errStr = "error", r.err.Error()
		log.Printf("scan: index %s: %v", r.rel, r.err)
	}
	_, err := a.index.Exec(`
INSERT INTO archives (path, identity, title, size, mtime, page_count, meta, category, has_cover, status, error, indexed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(path) DO UPDATE SET
    identity=excluded.identity, title=excluded.title, size=excluded.size, mtime=excluded.mtime,
    page_count=excluded.page_count, meta=excluded.meta, category=excluded.category, has_cover=excluded.has_cover,
    status=excluded.status, error=excluded.error, indexed_at=excluded.indexed_at`,
		r.rel, identityOf(r.rel), r.title, r.size, r.mt, r.pageCount, r.meta, r.category, b2i(r.hasCover), status, errStr, time.Now().Unix())
	if err != nil {
		log.Printf("scan: write %s: %v", r.rel, err)
		return
	}
	if !r.hasCover {
		os.Remove(a.coverPath(r.rel)) // clear any stale cover so handleCover won't serve it
	}
	// Refresh this archive's tag rows (identity by path -> id).
	var id int64
	if a.index.QueryRow(`SELECT id FROM archives WHERE path = ?`, r.rel).Scan(&id) != nil {
		return
	}
	a.index.Exec(`DELETE FROM tags WHERE archive_id = ?`, id)
	for _, t := range r.tags {
		a.index.Exec(`INSERT INTO tags (archive_id, namespace, tag) VALUES (?, ?, ?)`, id, t.ns, t.val)
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// scanLoop runs one scan at startup, then re-scans on the configured interval.
// Set SCAN_INTERVAL=0 to scan once and rely on the manual POST /api/scan.
func (a *App) scanLoop() {
	a.Scan()
	if a.cfg.ScanInterval <= 0 {
		return
	}
	t := time.NewTicker(a.cfg.ScanInterval)
	for range t.C {
		a.Scan()
	}
}
