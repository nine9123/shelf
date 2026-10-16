package main

import (
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
