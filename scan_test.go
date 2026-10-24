package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteResultReplacesTags(t *testing.T) {
	dir := t.TempDir()
	idx, err := openIndexDB(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	a := &App{cfg: Config{CoversDir: dir}, index: idx}

	a.writeResult(result{rel: "1 A.cbz", title: "A", pageCount: 3,
		tags: []tagPair{{"artist", "x"}, {"artist", "y"}}})
	a.writeResult(result{rel: "1 A.cbz", title: "A2", pageCount: 4,
		tags: []tagPair{{"group", "z"}}})

	var title, status string
	var pages, rows, tags int
	idx.QueryRow(`SELECT title, page_count, status FROM archives WHERE path='1 A.cbz'`).Scan(&title, &pages, &status)
	idx.QueryRow(`SELECT COUNT(*) FROM archives`).Scan(&rows)
	idx.QueryRow(`SELECT COUNT(*) FROM tags WHERE namespace='group' AND tag='z'`).Scan(&tags)
	if title != "A2" || pages != 4 || status != "indexed" || rows != 1 {
		t.Fatalf("row = %q %d %q (rows=%d)", title, pages, status, rows)
	}
	var total int
	idx.QueryRow(`SELECT COUNT(*) FROM tags`).Scan(&total)
	if tags != 1 || total != 1 {
		t.Fatalf("tags not replaced: group:z=%d total=%d", tags, total)
	}
}

// scanFixture builds an App over a fresh library and cache in a temp dir,
// with the worker pool running so Scan can index what it finds.
func scanFixture(t *testing.T) (*App, string) {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "library")
	cache := filepath.Join(dir, "cache")
	for _, d := range []string{lib, cache} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := openIndexDB(filepath.Join(cache, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	user, err := openUserDB(filepath.Join(cache, "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { user.Close() })
	a := &App{cfg: Config{RootDir: lib, CoversDir: cache, Workers: 1, ThumbWidth: 32},
		index: idx, user: user, jobs: make(chan job), results: make(chan result)}
	a.startWorkers()
	t.Cleanup(func() { close(a.jobs) })
	return a, lib
}

func writeTestZip(t *testing.T, p string) {
	t.Helper()
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("info.txt")
	w.Write([]byte("Tags: artist:x"))
	zw.Close()
	f.Close()
}

func archivePaths(t *testing.T, a *App) []string {
	t.Helper()
	rows, err := a.index.Query(`SELECT path FROM archives ORDER BY path`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		rows.Scan(&p)
		out = append(out, p)
	}
	return out
}

func TestRescanPrunesDeletedArchives(t *testing.T) {
	a, lib := scanFixture(t)
	writeTestZip(t, filepath.Join(lib, "1 A.cbz"))
	writeTestZip(t, filepath.Join(lib, "2 B.cbz"))
	a.Scan()
	if got := archivePaths(t, a); len(got) != 2 {
		t.Fatalf("after first scan: %v, want 2 rows", got)
	}

	os.Remove(filepath.Join(lib, "2 B.cbz"))
	a.Scan()
	got := archivePaths(t, a)
	if len(got) != 1 || got[0] != "1 A.cbz" {
		t.Fatalf("after deleting B: %v, want [1 A.cbz]", got)
	}
	var tags int
	a.index.QueryRow(`SELECT COUNT(*) FROM tags WHERE archive_id NOT IN (SELECT id FROM archives)`).Scan(&tags)
	if tags != 0 {
		t.Fatalf("%d orphaned tag rows left behind", tags)
	}
}

func TestRescanPrunesLastDeletedArchive(t *testing.T) {
	a, lib := scanFixture(t)
	writeTestZip(t, filepath.Join(lib, "1 A.cbz"))
	a.Scan()
	if got := archivePaths(t, a); len(got) != 1 {
		t.Fatalf("after first scan: %v, want 1 row", got)
	}

	// An empty library looks like a missing mount, so automatic scans keep the
	// rows; the rescan a user asks for trusts it and prunes them.
	os.Remove(filepath.Join(lib, "1 A.cbz"))
	a.Scan()
	if got := archivePaths(t, a); len(got) != 1 {
		t.Fatalf("automatic scan of an empty library pruned: %v", got)
	}
	a.Rescan()
	if got := archivePaths(t, a); len(got) != 0 {
		t.Fatalf("after deleting the only archive: %v, want none", got)
	}
}

func TestRescanKeepsRowsWhenRootMissing(t *testing.T) {
	a, lib := scanFixture(t)
	writeTestZip(t, filepath.Join(lib, "1 A.cbz"))
	a.Scan()
	os.RemoveAll(lib)
	a.Rescan()
	if got := archivePaths(t, a); len(got) != 1 {
		t.Fatalf("rescan with the library root gone pruned: %v", got)
	}
}

func TestRescanPrunesDanglingSymlink(t *testing.T) {
	a, lib := scanFixture(t)
	writeTestZip(t, filepath.Join(lib, "1 A.cbz"))
	target := filepath.Join(t.TempDir(), "b.cbz")
	writeTestZip(t, target)
	if err := os.Symlink(target, filepath.Join(lib, "2 B.cbz")); err != nil {
		t.Skip("symlinks unsupported:", err)
	}
	a.Scan()
	if got := archivePaths(t, a); len(got) != 2 {
		t.Fatalf("after first scan: %v, want 2 rows", got)
	}

	os.Remove(target) // the link stays, pointing at nothing
	a.Rescan()
	got := archivePaths(t, a)
	if len(got) != 1 || got[0] != "1 A.cbz" {
		t.Fatalf("after deleting the link target: %v, want [1 A.cbz]", got)
	}
}
