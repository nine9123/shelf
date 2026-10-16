package main

import (
	"archive/zip"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenZipPagesAndLazyMeta(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.cbz")
	f, _ := os.Create(p)
	zw := zip.NewWriter(f)
	for _, n := range []string{"p10.jpg", "info.txt", "p2.jpg", "dir/", "p1.png"} {
		w, _ := zw.Create(n)
		w.Write([]byte("x " + n))
	}
	zw.Close()
	f.Close()

	arc, err := openArchive(p)
	if err != nil {
		t.Fatal(err)
	}
	defer arc.Close()
	got := arc.Images()
	want := []string{"p1.png", "p2.jpg", "p10.jpg"}
	if len(got) != len(want) {
		t.Fatalf("images = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("images = %v, want %v", got, want)
		}
	}
	if m := arc.Meta(); m != "x info.txt" {
		t.Fatalf("meta = %q", m)
	}
}

func TestListingCacheReusesUntilFileChanges(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.cbr")
	os.WriteFile(p, []byte("v1"), 0o644)
	calls := 0
	lister := func(string) ([]string, error) {
		calls++
		return []string{"p10.jpg", "info.txt", "p2.jpg"}, nil
	}
	c := newListingCache(4)

	for i := 0; i < 3; i++ {
		l, err := c.get(p, lister)
		if err != nil {
			t.Fatal(err)
		}
		if len(l.images) != 2 || l.images[0] != "p2.jpg" || l.txtName != "info.txt" {
			t.Fatalf("listing = %+v", l)
		}
	}
	if calls != 1 {
		t.Fatalf("lister called %d times, want 1", calls)
	}

	later := time.Now().Add(time.Hour)
	os.Chtimes(p, later, later)
	c.get(p, lister)
	if calls != 2 {
		t.Fatalf("lister called %d times after mtime change, want 2", calls)
	}
}

func TestListingCacheSkipsErrorsAndEvicts(t *testing.T) {
	dir := t.TempDir()
	paths := make([]string, 3)
	for i := range paths {
		paths[i] = filepath.Join(dir, string(rune('a'+i))+".cbr")
		os.WriteFile(paths[i], nil, 0o644)
	}
	calls := 0
	fail := true
	lister := func(string) ([]string, error) {
		calls++
		if fail {
			return nil, errors.New("boom")
		}
		return []string{"1.jpg"}, nil
	}
	c := newListingCache(2)
	if _, err := c.get(paths[0], lister); err == nil {
		t.Fatal("want error")
	}
	fail = false
	c.get(paths[0], lister) // error wasn't cached
	c.get(paths[1], lister)
	c.get(paths[0], lister) // paths[1] is now least recently used
	c.get(paths[2], lister) // evicts paths[1]
	if calls != 4 {
		t.Fatalf("calls = %d, want 4", calls)
	}
	c.get(paths[0], lister)
	c.get(paths[1], lister)
	if calls != 5 {
		t.Fatalf("calls = %d, want 5 (only the evicted entry relisted)", calls)
	}
	if _, err := c.get(filepath.Join(dir, "missing.cbr"), lister); err == nil {
		t.Fatal("want stat error for missing file")
	}
}
