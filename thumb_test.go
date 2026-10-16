package main

import (
	"archive/zip"
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	pngenc "image/png"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
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

func TestHandleThumbScalesOrFallsBack(t *testing.T) {
	dir := t.TempDir()
	idx, err := openIndexDB(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	a := &App{cfg: Config{RootDir: dir}, index: idx}

	var png bytes.Buffer
	if err := pngenc.Encode(&png, testImages(480, 300)["rgba"]); err != nil {
		t.Fatal(err)
	}
	notAnImage := []byte("this is not decodable image data")
	f, _ := os.Create(filepath.Join(dir, "a.cbz"))
	zw := zip.NewWriter(f)
	for name, data := range map[string][]byte{"p1.png": png.Bytes(), "p2.webp": notAnImage} {
		w, _ := zw.Create(name)
		w.Write(data)
	}
	zw.Close()
	f.Close()
	a.writeResult(result{rel: "a.cbz", title: "a", pageCount: 2})
	var id string
	idx.QueryRow(`SELECT id FROM archives WHERE path='a.cbz'`).Scan(&id)

	get := func(n string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/archives/"+id+"/thumb/"+n, nil)
		req.SetPathValue("id", id)
		req.SetPathValue("n", n)
		rec := httptest.NewRecorder()
		a.handleThumb(rec, req)
		return rec
	}

	rec := get("0")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("page 0: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	cfg, err := jpeg.DecodeConfig(rec.Body)
	if err != nil || cfg.Width != 240 {
		t.Fatalf("page 0: thumb width %d, err %v", cfg.Width, err)
	}

	rec = get("1")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/webp" {
		t.Fatalf("page 1: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !bytes.Equal(rec.Body.Bytes(), notAnImage) {
		t.Fatalf("page 1: fallback body = %q", rec.Body.Bytes())
	}
}
