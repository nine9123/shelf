package main

import (
	"archive/zip"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var archiveExts = map[string]bool{".zip": true, ".cbz": true}

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
	zr, err := zip.OpenReader(j.abs)
	if err != nil {
		r.err = err
		return r
	}
	defer zr.Close()
	names := imageEntries(&zr.Reader)
	r.pageCount = len(names)
	r.meta = firstText(&zr.Reader)
	if len(names) > 0 {
		if rc, err := openEntry(&zr.Reader, names[0]); err == nil {
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
		if !archiveExts[strings.ToLower(filepath.Ext(d.Name()))] {
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

	// 3. anything new, changed, or not yet successfully indexed gets a job
	var todo []job
	for rel, fe := range disk {
		k, ok := known[rel]
		if !ok || k.size != fe.size || k.mt != fe.mt || k.status != "indexed" {
			todo = append(todo, job{rel: rel, abs: fe.abs, size: fe.size, mt: fe.mt})
		}
	}

	// 4. prune rows whose files are gone, and drop their covers
	for rel := range known {
		if _, ok := disk[rel]; !ok {
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
	log.Printf("scan: done in %s", time.Since(start).Round(time.Millisecond))
}

func (a *App) writeResult(r result) {
	status, errStr := "indexed", ""
	if r.err != nil {
		status, errStr = "error", r.err.Error()
	}
	_, err := a.index.Exec(`
INSERT INTO archives (path, title, size, mtime, page_count, meta, has_cover, status, error, indexed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(path) DO UPDATE SET
    title=excluded.title, size=excluded.size, mtime=excluded.mtime,
    page_count=excluded.page_count, meta=excluded.meta, has_cover=excluded.has_cover,
    status=excluded.status, error=excluded.error, indexed_at=excluded.indexed_at`,
		r.rel, r.title, r.size, r.mt, r.pageCount, r.meta, b2i(r.hasCover), status, errStr, time.Now().Unix())
	if err != nil {
		log.Printf("scan: write %s: %v", r.rel, err)
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
