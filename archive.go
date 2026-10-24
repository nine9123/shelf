package main

import (
	"archive/zip"
	"bytes"
	"container/list"
	"errors"
	"io"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true,
	".gif": true, ".webp": true, ".bmp": true, ".avif": true,
}

func isImageName(name string) bool {
	return !isMacJunk(name) && imageExts[strings.ToLower(path.Ext(name))]
}

// isMacJunk reports whether an entry is metadata macOS adds when compressing:
// AppleDouble "._name" resource-fork files (often under "__MACOSX/") and
// ".DS_Store". These carry an image's extension but are not images, so
// counting them inflates the page count with unreadable pages.
func isMacJunk(name string) bool {
	name = strings.ReplaceAll(name, "\\", "/")
	base := path.Base(name)
	if strings.HasPrefix(base, "._") || base == ".DS_Store" {
		return true
	}
	for _, part := range strings.Split(name, "/") {
		if part == "__MACOSX" {
			return true
		}
	}
	return false
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
		} else if metaFile == nil && !isMacJunk(f.Name) && strings.EqualFold(path.Ext(f.Name), ".txt") {
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
// The listing itself is cached (listingCache), so a page turn costs one pass.

func openLibarchive(absPath string) (*pageArchive, error) {
	st, err := os.Stat(absPath)
	if err != nil {
		return nil, err
	}
	l, err := libarchiveListings.get(absPath, st.ModTime(), st.Size(), func() (*listing, error) {
		return loadListing(absPath)
	})
	if err != nil {
		return nil, err
	}
	images, txtName := l.images, l.txtName
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

// listing is the parsed table of contents of a bsdtar-backed archive. It is
// shared between openers via listingCache and must not be modified.
type listing struct {
	images  []string
	txtName string
}

func loadListing(absPath string) (*listing, error) {
	names, err := bsdtarList(absPath)
	if err != nil {
		return nil, err
	}
	l := &listing{}
	for _, n := range names {
		if isImageName(n) {
			l.images = append(l.images, n)
		} else if l.txtName == "" && !isMacJunk(n) && strings.EqualFold(path.Ext(n), ".txt") {
			l.txtName = n
		}
	}
	sortNatural(l.images)
	return l, nil
}

// listingCache remembers recent bsdtar listings so that serving a page costs
// one pass over the archive (the extraction) instead of two. Entries are keyed
// by path and validated against modification time and size, so a replaced or
// rewritten archive is listed afresh. Failed listings are not cached.
type listingCache struct {
	mu    sync.Mutex
	max   int
	ll    *list.List // front = most recently used; values are *listingEntry
	byKey map[string]*list.Element
}

type listingEntry struct {
	path string
	mt   time.Time
	size int64
	l    *listing
}

// libarchiveListings is sized for a handful of archives being read at once;
// a library scan cycles through it, which is harmless.
var libarchiveListings = newListingCache(64)

func newListingCache(max int) *listingCache {
	return &listingCache{max: max, ll: list.New(), byKey: map[string]*list.Element{}}
}

func (c *listingCache) get(absPath string, mt time.Time, size int64, load func() (*listing, error)) (*listing, error) {
	c.mu.Lock()
	if el, ok := c.byKey[absPath]; ok {
		e := el.Value.(*listingEntry)
		if e.mt.Equal(mt) && e.size == size {
			c.ll.MoveToFront(el)
			c.mu.Unlock()
			return e.l, nil
		}
		c.ll.Remove(el)
		delete(c.byKey, absPath)
	}
	c.mu.Unlock()

	// List outside the lock: it runs bsdtar over the whole archive. Concurrent
	// misses on the same path may both list it; the last one to finish wins.
	l, err := load()
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.byKey[absPath]; ok {
		c.ll.Remove(el)
	}
	c.byKey[absPath] = c.ll.PushFront(&listingEntry{path: absPath, mt: mt, size: size, l: l})
	for c.ll.Len() > c.max {
		old := c.ll.Back()
		c.ll.Remove(old)
		delete(c.byKey, old.Value.(*listingEntry).path)
	}
	return l, nil
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
