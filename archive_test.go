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

func TestListingCacheHitsAndInvalidates(t *testing.T) {
	c := newListingCache(2)
	loads := 0
	load := func() (*listing, error) {
		loads++
		return &listing{images: []string{"p1.jpg"}}, nil
	}
	t0 := time.Unix(1000, 0)

	c.get("/a.cbr", t0, 10, load)
	c.get("/a.cbr", t0, 10, load)
	if loads != 1 {
		t.Fatalf("same path+mtime+size: loads = %d, want 1", loads)
	}
	c.get("/a.cbr", t0.Add(time.Second), 10, load)
	if loads != 2 {
		t.Fatalf("new mtime: loads = %d, want 2", loads)
	}
	c.get("/a.cbr", t0.Add(time.Second), 11, load)
	if loads != 3 {
		t.Fatalf("new size: loads = %d, want 3", loads)
	}

	// /a.cbr is least recently used once /b and /c are added, so it is evicted.
	c.get("/b.cbr", t0, 10, load)
	c.get("/c.cbr", t0, 10, load)
	if c.ll.Len() != 2 {
		t.Fatalf("len = %d, want 2", c.ll.Len())
	}
	c.get("/a.cbr", t0.Add(time.Second), 11, load)
	if loads != 6 {
		t.Fatalf("after eviction: loads = %d, want 6", loads)
	}

	// Failures are returned and not cached.
	_, err := c.get("/bad.cbr", t0, 1, func() (*listing, error) { return nil, errors.New("boom") })
	if err == nil {
		t.Fatal("want error")
	}
	if _, ok := c.byKey["/bad.cbr"]; ok {
		t.Fatal("failed listing was cached")
	}
}
