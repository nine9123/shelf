package main

import (
	"bytes"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
)

// makeThumb decodes an image, scales it to width w (preserving aspect ratio,
// never upscaling), and writes JPEG to dst. The file is written only on success,
// so a decode failure never leaves an empty cover behind, and it is written to a
// temp file and renamed into place, so a crash or SIGKILL mid-write never leaves
// a truncated cover that later gets served as valid.
//
// This uses a hand-rolled bilinear filter so the build stays pure stdlib with
// no cgo image libraries. It is fine for a first pass; for a full 2 TB library
// swap this for libvips (govips) — see the README — which is far faster and can
// emit WebP.
func makeThumb(r io.Reader, w int, dst string) error {
	var buf bytes.Buffer
	if err := makeThumbTo(r, w, &buf); err != nil {
		return err
	}
	return writeFileAtomic(dst, buf.Bytes(), 0o644)
}

// writeFileAtomic writes data to a temp file next to dst and renames it over
// dst. Temp names match ".*.tmp" so sweepTempCovers can clear leftovers.
func writeFileAtomic(dst string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(perm)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// makeThumbTo decodes, scales to width w, and writes JPEG to out.
func makeThumbTo(r io.Reader, w int, out io.Writer) error {
	src, _, err := image.Decode(r)
	if err != nil {
		return err
	}
	return jpeg.Encode(out, scaleWidth(src, w), &jpeg.Options{Quality: 80})
}

func scaleWidth(src image.Image, targetW int) image.Image {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 || targetW >= sw {
		return src // nothing to do, or would upscale
	}
	targetH := sh * targetW / sw
	if targetH < 1 {
		targetH = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, targetW, targetH))
	at := pixelSampler(src)
	xr := float64(sw) / float64(targetW)
	yr := float64(sh) / float64(targetH)
	for y := 0; y < targetH; y++ {
		fy := (float64(y)+0.5)*yr - 0.5
		y0 := int(fy)
		wy := fy - float64(y0)
		for x := 0; x < targetW; x++ {
			fx := (float64(x)+0.5)*xr - 0.5
			x0 := int(fx)
			wx := fx - float64(x0)
			r00, g00, b00 := sample(at, b, x0, y0)
			r10, g10, b10 := sample(at, b, x0+1, y0)
			r01, g01, b01 := sample(at, b, x0, y0+1)
			r11, g11, b11 := sample(at, b, x0+1, y0+1)
			i := dst.PixOffset(x, y)
			dst.Pix[i+0] = clamp8(bilerp(r00, r10, r01, r11, wx, wy))
			dst.Pix[i+1] = clamp8(bilerp(g00, g10, g01, g11, wx, wy))
			dst.Pix[i+2] = clamp8(bilerp(b00, b10, b01, b11, wx, wy))
			dst.Pix[i+3] = 255
		}
	}
	return dst
}

// pixelSampler returns a function equivalent to img.At(x, y).RGBA(), but for the
// concrete types the decoders actually produce it reads the pixel without
// boxing a color.Color per call. The generic At path allocates on every sample
// (four per output pixel), which dominated thumbnailing time and GC churn.
func pixelSampler(img image.Image) func(x, y int) (r, g, b, a uint32) {
	switch m := img.(type) {
	case *image.YCbCr: // JPEG
		return func(x, y int) (uint32, uint32, uint32, uint32) { return m.YCbCrAt(x, y).RGBA() }
	case *image.RGBA:
		return func(x, y int) (uint32, uint32, uint32, uint32) { return m.RGBAAt(x, y).RGBA() }
	case *image.NRGBA: // PNG, WebP (lossless/alpha)
		return func(x, y int) (uint32, uint32, uint32, uint32) { return m.NRGBAAt(x, y).RGBA() }
	case *image.Gray:
		return func(x, y int) (uint32, uint32, uint32, uint32) { return m.GrayAt(x, y).RGBA() }
	}
	return func(x, y int) (uint32, uint32, uint32, uint32) { return img.At(x, y).RGBA() }
}

func sample(at func(x, y int) (r, g, b, a uint32), b image.Rectangle, x, y int) (float64, float64, float64) {
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if x > b.Dx()-1 {
		x = b.Dx() - 1
	}
	if y > b.Dy()-1 {
		y = b.Dy() - 1
	}
	r, g, bb, _ := at(b.Min.X+x, b.Min.Y+y)
	return float64(r >> 8), float64(g >> 8), float64(bb >> 8)
}

func bilerp(v00, v10, v01, v11, wx, wy float64) float64 {
	top := v00*(1-wx) + v10*wx
	bot := v01*(1-wx) + v11*wx
	return top*(1-wy) + bot*wy
}

func clamp8(v float64) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v + 0.5)
}
