package main

import (
	"archive/zip"
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// atOnly hides an image's concrete type, forcing scaleWidth onto the generic
// image.At path. The fast samplers must match it pixel for pixel.
type atOnly struct{ image.Image }

func testImages(w, h int) map[string]image.Image {
	ycc := image.NewYCbCr(image.Rect(3, 5, 3+w, 5+h), image.YCbCrSubsampleRatio420)
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	nrgba := image.NewNRGBA(image.Rect(0, 0, w, h))
	gray := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8((x*7 + y*13) % 256)
			ycc.Y[ycc.YOffset(3+x, 5+y)] = v
			ycc.Cb[ycc.COffset(3+x, 5+y)] = uint8((x * 3) % 256)
			ycc.Cr[ycc.COffset(3+x, 5+y)] = uint8((y * 5) % 256)
			rgba.SetRGBA(x, y, color.RGBA{v, v / 2, 255 - v, 255})
			nrgba.SetNRGBA(x, y, color.NRGBA{v, v / 3, 255 - v, uint8(x % 256)})
			gray.SetGray(x, y, color.Gray{v})
		}
	}
	return map[string]image.Image{"ycbcr": ycc, "rgba": rgba, "nrgba": nrgba, "gray": gray}
}

func TestScaleWidthFastPathMatchesGeneric(t *testing.T) {
	for name, img := range testImages(97, 61) {
		fast := scaleWidth(img, 40).(*image.RGBA)
		slow := scaleWidth(atOnly{img}, 40).(*image.RGBA)
		if fast.Bounds() != slow.Bounds() {
			t.Fatalf("%s: bounds %v != %v", name, fast.Bounds(), slow.Bounds())
		}
		for i := range fast.Pix {
			if fast.Pix[i] != slow.Pix[i] {
				t.Fatalf("%s: pixel byte %d: fast %d != generic %d", name, i, fast.Pix[i], slow.Pix[i])
			}
		}
	}
}

func BenchmarkScaleWidthYCbCr(b *testing.B) {
	img := testImages(1600, 2400)["ycbcr"]
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		scaleWidth(img, 400)
	}
}

func BenchmarkScaleWidthGeneric(b *testing.B) {
	img := atOnly{testImages(1600, 2400)["ycbcr"]}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		scaleWidth(img, 400)
	}
}

func TestHandleThumbStreamsAndFallsBack(t *testing.T) {
	dir := t.TempDir()
	var jpg bytes.Buffer
	jpeg.Encode(&jpg, testImages(400, 300)["rgba"], nil)
	unknown := []byte("not an image we can decode")
	corrupt := append([]byte{}, jpg.Bytes()[:200]...) // valid header, truncated body

	zf, _ := os.Create(filepath.Join(dir, "a.cbz"))
	zw := zip.NewWriter(zf)
	for _, p := range []struct {
		name string
		data []byte
	}{{"1.jpg", jpg.Bytes()}, {"2.png", unknown}, {"3.jpg", corrupt}} {
		w, _ := zw.Create(p.name)
		w.Write(p.data)
	}
	zw.Close()
	zf.Close()

	idx, err := openIndexDB(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	idx.Exec(`INSERT INTO archives(path, identity, title, page_count, has_cover, status, indexed_at) VALUES(?,?,?,?,?,?,?)`,
		"a.cbz", "a", "a", 3, 0, "indexed", time.Now().Unix())
	a := &App{cfg: Config{RootDir: dir}, index: idx}

	get := func(n string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/archives/1/thumb/"+n, nil)
		r.SetPathValue("id", "1")
		r.SetPathValue("n", n)
		rec := httptest.NewRecorder()
		a.handleThumb(rec, r)
		return rec
	}

	rec := get("0")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("page 0: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if cfg, err := jpeg.DecodeConfig(rec.Body); err != nil || cfg.Width != 240 {
		t.Fatalf("page 0 thumb: %+v %v", cfg, err)
	}

	rec = get("1")
	if rec.Header().Get("Content-Type") != "image/png" || !bytes.Equal(rec.Body.Bytes(), unknown) {
		t.Fatalf("page 1 fallback: %q %q", rec.Header().Get("Content-Type"), rec.Body.Bytes())
	}

	rec = get("2")
	if rec.Header().Get("Content-Type") != "image/jpeg" || !bytes.Equal(rec.Body.Bytes(), corrupt) {
		t.Fatalf("page 2 fallback: %q, %d bytes (want %d)", rec.Header().Get("Content-Type"), rec.Body.Len(), len(corrupt))
	}
}
