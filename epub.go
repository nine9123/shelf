package main

import (
	"encoding/xml"
	"io"
	"net/url"
	"path"
	"strings"
)

// EPUB is a zip with a manifest. Shelf reads it as a page archive: the pages
// are the images in the book's reading order, found by walking the spine (the
// OPF's list of content documents) and collecting the images each document
// shows. That is what makes fixed-layout and image-only EPUBs (comics, manga,
// picture books) read correctly, since their image filenames often don't sort
// into reading order. A text EPUB yields only its illustrations; Shelf has no
// text reader.
//
// If the manifest can't be read, or the spine references no images, the pages
// fall back to every image in the zip in natural order, as for a .cbz.

func openEpub(absPath string) (*pageArchive, error) {
	arc, err := openZip(absPath)
	if err != nil {
		return nil, err
	}
	pkg := readEpubPackage(arc)
	if pkg == nil {
		return arc, nil
	}
	if pages := pkg.spinePages(arc); len(pages) > 0 {
		arc.images = pages
	}
	zipMeta := arc.metaFn
	arc.metaFn = func() string {
		if m := zipMeta(); m != "" {
			return m // an info.txt inside the book wins, as in any archive
		}
		return pkg.info()
	}
	return arc, nil
}

const epubMaxXML = 8 << 20 // cap on any one container/OPF/XHTML document read

// epubPackage is the parsed OPF: the manifest, the spine, and the Dublin Core
// metadata. Hrefs are resolved to zip entry names.
type epubPackage struct {
	items map[string]epubItem // manifest id -> item
	spine []string            // manifest ids in reading order

	title, creators, subjects, publisher, language []string
}

type epubItem struct {
	name      string // zip entry name
	mediaType string
}

func readEpubPackage(arc *pageArchive) *epubPackage {
	var container struct {
		Rootfiles []struct {
			FullPath  string `xml:"full-path,attr"`
			MediaType string `xml:"media-type,attr"`
		} `xml:"rootfiles>rootfile"`
	}
	if !readEpubXML(arc, "META-INF/container.xml", &container) {
		return nil
	}
	opfName := ""
	for _, rf := range container.Rootfiles {
		if rf.FullPath != "" && (opfName == "" || rf.MediaType == "application/oebps-package+xml") {
			opfName = rf.FullPath
		}
	}
	if opfName == "" {
		return nil
	}

	var opf struct {
		Metadata struct {
			Title     []string `xml:"title"`
			Creator   []string `xml:"creator"`
			Subject   []string `xml:"subject"`
			Publisher []string `xml:"publisher"`
			Language  []string `xml:"language"`
		} `xml:"metadata"`
		Items []struct {
			ID        string `xml:"id,attr"`
			Href      string `xml:"href,attr"`
			MediaType string `xml:"media-type,attr"`
		} `xml:"manifest>item"`
		Itemrefs []struct {
			IDRef string `xml:"idref,attr"`
		} `xml:"spine>itemref"`
	}
	if !readEpubXML(arc, opfName, &opf) {
		return nil
	}
	p := &epubPackage{
		items:     make(map[string]epubItem, len(opf.Items)),
		title:     opf.Metadata.Title,
		creators:  opf.Metadata.Creator,
		subjects:  opf.Metadata.Subject,
		publisher: opf.Metadata.Publisher,
		language:  opf.Metadata.Language,
	}
	opfDir := path.Dir(opfName)
	for _, it := range opf.Items {
		if it.ID == "" || it.Href == "" {
			continue
		}
		p.items[it.ID] = epubItem{name: resolveEpubHref(opfDir, it.Href), mediaType: strings.ToLower(it.MediaType)}
	}
	for _, ref := range opf.Itemrefs {
		p.spine = append(p.spine, ref.IDRef)
	}
	return p
}

// spinePages lists the images shown by the spine, in order, without repeats.
// Only names that are image entries of the zip are kept.
func (p *epubPackage) spinePages(arc *pageArchive) []string {
	have := make(map[string]bool, len(arc.images))
	for _, n := range arc.images {
		have[n] = true
	}
	seen := map[string]bool{}
	var pages []string
	add := func(n string) {
		if have[n] && !seen[n] {
			seen[n] = true
			pages = append(pages, n)
		}
	}
	for _, id := range p.spine {
		it, ok := p.items[id]
		if !ok {
			continue
		}
		if strings.HasPrefix(it.mediaType, "image/") && it.mediaType != "image/svg+xml" {
			add(it.name)
			continue
		}
		for _, n := range epubDocImages(arc, it.name) {
			add(n)
		}
	}
	return pages
}

// epubDocImages returns the images an XHTML (or SVG) content document
// references through <img src> or SVG <image href / xlink:href>, resolved to
// zip entry names, in document order. The parser is lenient, since real-world
// EPUB markup is often not well-formed XML.
func epubDocImages(arc *pageArchive, name string) []string {
	rc, err := arc.Open(name)
	if err != nil {
		return nil
	}
	defer rc.Close()
	dec := xml.NewDecoder(io.LimitReader(rc, epubMaxXML))
	dec.Strict = false
	dec.AutoClose = xml.HTMLAutoClose
	dec.Entity = xml.HTMLEntity
	dir := path.Dir(name)
	var out []string
	for {
		tok, err := dec.Token()
		if err != nil {
			return out
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		var want string
		switch strings.ToLower(se.Name.Local) {
		case "img":
			want = "src"
		case "image":
			want = "href" // SVG 2 href or SVG 1.1 xlink:href; both have local name "href"
		default:
			continue
		}
		for _, a := range se.Attr {
			if strings.EqualFold(a.Name.Local, want) && a.Value != "" {
				out = append(out, resolveEpubHref(dir, a.Value))
				break
			}
		}
	}
}

// resolveEpubHref turns a (relative, URL-encoded) href found in a document in
// dir into a zip entry name. Fragments and queries are dropped.
func resolveEpubHref(dir, href string) string {
	if i := strings.IndexAny(href, "#?"); i >= 0 {
		href = href[:i]
	}
	if u, err := url.PathUnescape(href); err == nil {
		href = u
	}
	if strings.HasPrefix(href, "/") {
		return strings.TrimPrefix(path.Clean(href), "/")
	}
	return strings.TrimPrefix(path.Clean(path.Join(dir, href)), "./")
}

func readEpubXML(arc *pageArchive, name string, v any) bool {
	rc, err := arc.Open(name)
	if err != nil {
		return false
	}
	defer rc.Close()
	dec := xml.NewDecoder(io.LimitReader(rc, epubMaxXML))
	dec.Strict = false
	dec.Entity = xml.HTMLEntity
	return dec.Decode(v) == nil
}

// info renders the book's Dublin Core metadata in the info.txt format that
// parseInfo understands, so authors and subjects become searchable tags.
func (p *epubPackage) info() string {
	var b strings.Builder
	if t := epubJoin(p.title); t != "" {
		b.WriteString("Title: " + t + "\n")
	}
	for _, f := range []struct {
		ns   string
		vals []string
	}{
		{"author", p.creators},
		{"subject", p.subjects},
		{"publisher", p.publisher},
		{"language", p.language},
	} {
		if v := epubJoin(f.vals); v != "" {
			b.WriteString("> " + f.ns + ": " + v + "\n")
		}
	}
	return b.String()
}

// epubJoin trims and comma-joins values. Commas inside one value would split
// it into several tags, so they are replaced by spaces.
func epubJoin(vals []string) string {
	var out []string
	for _, v := range vals {
		v = strings.Join(strings.Fields(strings.ReplaceAll(v, ",", " ")), " ")
		if v != "" {
			out = append(out, v)
		}
	}
	return strings.Join(out, ", ")
}
