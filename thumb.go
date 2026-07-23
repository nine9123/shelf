package main

import (
	"bytes"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"os"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
)

// makeThumb decodes an image, scales it to width w (preserving aspect ratio,
// never upscaling), and writes JPEG to dst. The file is written only on success,
// so a decode failure never leaves an empty cover behind.
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
	return os.WriteFile(dst, buf.Bytes(), 0o644)
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
			r00, g00, b00 := sample(src, b, x0, y0)
			r10, g10, b10 := sample(src, b, x0+1, y0)
			r01, g01, b01 := sample(src, b, x0, y0+1)
			r11, g11, b11 := sample(src, b, x0+1, y0+1)
			i := dst.PixOffset(x, y)
			dst.Pix[i+0] = clamp8(bilerp(r00, r10, r01, r11, wx, wy))
			dst.Pix[i+1] = clamp8(bilerp(g00, g10, g01, g11, wx, wy))
			dst.Pix[i+2] = clamp8(bilerp(b00, b10, b01, b11, wx, wy))
			dst.Pix[i+3] = 255
		}
	}
	return dst
}

func sample(img image.Image, b image.Rectangle, x, y int) (float64, float64, float64) {
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
	r, g, bb, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
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
