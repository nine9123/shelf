package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeZip(t *testing.T, p string, files [][2]string) {
	t.Helper()
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, e := range files {
		w, _ := zw.Create(e[0])
		w.Write([]byte(e[1]))
	}
	zw.Close()
	f.Close()
}

const testContainer = `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`

const testOPF = `<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>My Book</dc:title>
    <dc:creator>Jane Doe</dc:creator>
    <dc:creator>Doe, John</dc:creator>
    <dc:subject>Fantasy</dc:subject>
    <dc:language>en</dc:language>
  </metadata>
  <manifest>
    <item id="cover" href="Text/cover.xhtml" media-type="application/xhtml+xml"/>
    <item id="p1" href="Text/page%201.xhtml" media-type="application/xhtml+xml"/>
    <item id="p2" href="Text/p2.xhtml" media-type="application/xhtml+xml"/>
    <item id="raw" href="Images/z-last.png" media-type="image/png"/>
    <item id="nav" href="nav.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine>
    <itemref idref="cover" linear="no"/>
    <itemref idref="p1"/>
    <itemref idref="p2"/>
    <itemref idref="raw"/>
    <itemref idref="missing"/>
    <itemref idref="nav"/>
  </spine>
</package>`

func TestOpenEpubSpineOrderAndMeta(t *testing.T) {
	p := filepath.Join(t.TempDir(), "book.EPUB")
	writeZip(t, p, [][2]string{
		{"mimetype", "application/epub+zip"},
		{"META-INF/container.xml", testContainer},
		{"OEBPS/content.opf", testOPF},
		// SVG-wrapped cover, as fixed-layout books commonly do.
		{"OEBPS/Text/cover.xhtml", `<html xmlns="http://www.w3.org/1999/xhtml"><body>
			<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink">
			<image xlink:href="../Images/cover.jpg"/></svg></body></html>`},
		// HTML-ish markup (unclosed <br>, &nbsp;) and an encoded name; the
		// same image twice counts once.
		{"OEBPS/Text/page 1.xhtml", `<html><body><p>&nbsp;<br></p>
			<img src="../Images/b%20page.jpg" alt=""><img src="../Images/b%20page.jpg#x"></body></html>`},
		{"OEBPS/Text/p2.xhtml", `<html><body><img src="../Images/a.webp"/><img src="../Images/gone.jpg"/></body></html>`},
		{"OEBPS/nav.xhtml", `<html><body><nav>no images</nav></body></html>`},
		{"OEBPS/Images/cover.jpg", "c"},
		{"OEBPS/Images/b page.jpg", "b"},
		{"OEBPS/Images/a.webp", "a"},
		{"OEBPS/Images/z-last.png", "z"},
		{"OEBPS/Images/unused.jpg", "u"},
	})
	if indexed, _ := archiveMode(p); !indexed {
		t.Fatal("epub not indexed")
	}
	arc, err := openArchive(p)
	if err != nil {
		t.Fatal(err)
	}
	defer arc.Close()
	want := []string{"OEBPS/Images/cover.jpg", "OEBPS/Images/b page.jpg", "OEBPS/Images/a.webp", "OEBPS/Images/z-last.png"}
	if got := arc.Images(); !reflect.DeepEqual(got, want) {
		t.Fatalf("images = %q, want %q", got, want)
	}
	rc, err := arc.Open(want[1])
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()

	cat, tags := parseInfo(arc.Meta())
	wantTags := []tagPair{{"author", "Jane Doe"}, {"author", "Doe John"}, {"subject", "Fantasy"}, {"language", "en"}}
	if cat != "" || !reflect.DeepEqual(tags, wantTags) {
		t.Fatalf("meta %q parsed to %q %v", arc.Meta(), cat, tags)
	}
}

func TestOpenEpubFallsBackToNaturalOrder(t *testing.T) {
	// No container.xml: read like a plain zip.
	p := filepath.Join(t.TempDir(), "bare.epub")
	writeZip(t, p, [][2]string{{"p10.jpg", "x"}, {"p2.jpg", "x"}, {"info.txt", "Category: Manga"}})
	arc, err := openArchive(p)
	if err != nil {
		t.Fatal(err)
	}
	defer arc.Close()
	if got := arc.Images(); !reflect.DeepEqual(got, []string{"p2.jpg", "p10.jpg"}) {
		t.Fatalf("images = %q", got)
	}
	if m := arc.Meta(); m != "Category: Manga" {
		t.Fatalf("meta = %q", m)
	}

	// Valid package whose spine shows no images (a text book): all images.
	p = filepath.Join(t.TempDir(), "text.epub")
	writeZip(t, p, [][2]string{
		{"META-INF/container.xml", testContainer},
		{"OEBPS/content.opf", `<package><manifest><item id="c" href="c.xhtml" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="c"/></spine></package>`},
		{"OEBPS/c.xhtml", `<html><body>words</body></html>`},
		{"OEBPS/fig2.png", "x"}, {"OEBPS/fig1.png", "x"},
	})
	arc2, err := openArchive(p)
	if err != nil {
		t.Fatal(err)
	}
	defer arc2.Close()
	if got := arc2.Images(); !reflect.DeepEqual(got, []string{"OEBPS/fig1.png", "OEBPS/fig2.png"}) {
		t.Fatalf("images = %q", got)
	}
}
