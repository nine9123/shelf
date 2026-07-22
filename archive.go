package main

import (
	"archive/zip"
	"io"
	"path"
	"sort"
	"strings"
)

var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true,
	".gif": true, ".webp": true, ".bmp": true,
}

func isImageName(name string) bool {
	return imageExts[strings.ToLower(path.Ext(name))]
}

// imageEntries returns the archive's image file names in natural reading order.
// Reading the central directory is cheap (it lives at the end of the file), so
// this does not decompress anything.
func imageEntries(zr *zip.Reader) []string {
	var names []string
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if isImageName(f.Name) {
			names = append(names, f.Name)
		}
	}
	sort.Slice(names, func(i, j int) bool { return naturalLess(names[i], names[j]) })
	return names
}

// firstText returns the contents of the first .txt entry (capped at 1 MB), or "".
func firstText(zr *zip.Reader) string {
	for _, f := range zr.File {
		if strings.ToLower(path.Ext(f.Name)) == ".txt" {
			rc, err := f.Open()
			if err != nil {
				return ""
			}
			b, _ := io.ReadAll(io.LimitReader(rc, 1<<20))
			rc.Close()
			return string(b)
		}
	}
	return ""
}

// openEntry returns a reader for a single named entry, decompressing only that
// one page rather than the whole archive.
func openEntry(zr *zip.Reader, name string) (io.ReadCloser, error) {
	for _, f := range zr.File {
		if f.Name == name {
			return f.Open()
		}
	}
	return nil, io.EOF
}
