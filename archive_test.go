package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
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
