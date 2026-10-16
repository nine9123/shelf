package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os/exec"
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

// archiveMode reports whether a filename is an archive we index, and whether it
// is handled natively (zip) or via libarchive/bsdtar (everything else).
func archiveMode(name string) (indexed bool, useZip bool) {
	l := strings.ToLower(name)
	switch {
	case hasAnySuffix(l, ".zip", ".cbz"):
		return true, true
	case hasAnySuffix(l, ".rar", ".cbr", ".7z", ".cb7", ".tar", ".cbt",
		".tar.gz", ".tgz", ".tar.xz", ".txz", ".tar.bz2", ".tbz2"):
		return true, false
	}
	return false, false
}

func hasAnySuffix(s string, suffixes ...string) bool {
	for _, x := range suffixes {
		if strings.HasSuffix(s, x) {
			return true
		}
	}
	return false
}

func sortNatural(names []string) {
	sort.Slice(names, func(i, j int) bool { return naturalLess(names[i], names[j]) })
}

// pageArchive is a uniform view of one archive's contents, independent of the
// container format: the image page names in reading order, the first .txt as
// metadata, and a way to open any single entry as a stream.
//
// The metadata is read lazily: only the indexer needs it, while serving a page
// or thumbnail opens the archive too, and for bsdtar formats reading the .txt
// costs an extra process and a pass over the archive.
type pageArchive struct {
	images  []string
	metaFn  func() string
	openFn  func(name string) (io.ReadCloser, error)
	closeFn func() error
}

func (p *pageArchive) Images() []string { return p.images }
func (p *pageArchive) Meta() string {
	if p.metaFn == nil {
		return ""
	}
	return p.metaFn()
}
func (p *pageArchive) Open(name string) (io.ReadCloser, error) { return p.openFn(name) }
func (p *pageArchive) Close() error {
	if p.closeFn != nil {
		return p.closeFn()
	}
	return nil
}

func openArchive(absPath string) (*pageArchive, error) {
	if _, useZip := archiveMode(absPath); useZip {
		return openZip(absPath)
	}
	return openLibarchive(absPath)
}

// --- native zip backend: fast, random access, no external tools ---

func openZip(absPath string) (*pageArchive, error) {
	zr, err := zip.OpenReader(absPath)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]*zip.File, len(zr.File))
	var images []string
	var metaFile *zip.File
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		byName[f.Name] = f
		if isImageName(f.Name) {
			images = append(images, f.Name)
		} else if metaFile == nil && strings.EqualFold(path.Ext(f.Name), ".txt") {
			metaFile = f
		}
	}
	sortNatural(images)
	return &pageArchive{
		images: images,
		metaFn: func() string {
			if metaFile == nil {
				return ""
			}
			rc, err := metaFile.Open()
			if err != nil {
				return ""
			}
			defer rc.Close()
			b, _ := io.ReadAll(io.LimitReader(rc, 1<<20))
			return string(b)
		},
		openFn: func(n string) (io.ReadCloser, error) {
			f, ok := byName[n]
			if !ok {
				return nil, io.EOF
			}
			return f.Open()
		},
		closeFn: zr.Close,
	}, nil
}

// --- libarchive backend: rar, 7z, and the tar family ---
//
// This shells out to `bsdtar` (the libarchive CLI), which auto-detects the
// format and reads all of these container types read-only, including RAR and
// 7z. tar-family and solid archives have no random access, so a page is
// produced by streaming the archive until the requested entry — fine for the
// modest archive sizes here; see the README for the in-memory-cache upgrade.

func openLibarchive(absPath string) (*pageArchive, error) {
	names, err := bsdtarList(absPath)
	if err != nil {
		return nil, err
	}
	var images []string
	txtName := ""
	for _, n := range names {
		if isImageName(n) {
			images = append(images, n)
		} else if txtName == "" && strings.EqualFold(path.Ext(n), ".txt") {
			txtName = n
		}
	}
	sortNatural(images)
	return &pageArchive{
		images: images,
		metaFn: func() string {
			if txtName == "" {
				return ""
			}
			rc, err := bsdtarExtract(absPath, txtName)
			if err != nil {
				return ""
			}
			defer rc.Close()
			b, _ := io.ReadAll(io.LimitReader(rc, 1<<20))
			return string(b)
		},
		openFn: func(n string) (io.ReadCloser, error) { return bsdtarExtract(absPath, n) },
	}, nil
}

func bsdtarList(absPath string) ([]string, error) {
	cmd := exec.Command("bsdtar", "-tf", absPath)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if msg := firstLine(errb.String()); msg != "" {
			return nil, errors.New(msg) // e.g. "bsdtar: Error opening archive: Unrecognized archive format"
		}
		return nil, err
	}
	var names []string
	for _, n := range strings.Split(out.String(), "\n") {
		n = strings.TrimRight(n, "\r")
		if n == "" || strings.HasSuffix(n, "/") {
			continue
		}
		names = append(names, n)
	}
	return names, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// bsdtarPattern escapes shell-glob metacharacters so an entry name is matched
// literally. bsdtar treats the operand as a pattern, and names containing
// [ ] * ? (very common in these archives) would otherwise fail to match.
func bsdtarPattern(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch r {
		case '\\', '*', '?', '[', ']':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// bsdtarExtract streams a single entry to stdout. Closing the returned reader
// stops and reaps the child process.
func bsdtarExtract(absPath, entry string) (io.ReadCloser, error) {
	cmd := exec.Command("bsdtar", "-x", "-O", "-f", absPath, "--", bsdtarPattern(entry))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &cmdReader{r: stdout, cmd: cmd}, nil
}

type cmdReader struct {
	r   io.ReadCloser
	cmd *exec.Cmd
}

func (c *cmdReader) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *cmdReader) Close() error {
	c.r.Close() // unblock the child if we stopped reading early
	return c.cmd.Wait()
}
