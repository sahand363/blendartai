// Package imageprep loads a source image and shrinks it for sending to an AI.
package imageprep

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"

	_ "golang.org/x/image/webp" // decode WebP sources
)

// MaxSide is the longest side sent to the models.
const MaxSide = 1024

type Image struct {
	Data     []byte
	MIMEType string
}

// Load decodes a PNG, JPEG or WebP file, scales it so the longest side is at
// most MaxSide, and encodes PNG (for PNG sources) or JPEG.
func Load(path string) (Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return Image{}, err
	}
	defer f.Close()
	src, format, err := image.Decode(f)
	if err != nil {
		return Image{}, fmt.Errorf("decode %s: %w", path, err)
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > MaxSide || h > MaxSide {
		nw, nh := MaxSide, MaxSide
		if w >= h {
			nh = max(1, h*MaxSide/w)
		} else {
			nw = max(1, w*MaxSide/h)
		}
		src = resize(src, nw, nh)
	}
	var buf bytes.Buffer
	if format == "png" {
		if err := png.Encode(&buf, src); err != nil {
			return Image{}, err
		}
		return Image{Data: buf.Bytes(), MIMEType: "image/png"}, nil
	}
	if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: 90}); err != nil {
		return Image{}, err
	}
	return Image{Data: buf.Bytes(), MIMEType: "image/jpeg"}, nil
}

func resize(src image.Image, dw, dh int) *image.RGBA {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	xr := float64(sw-1) / float64(max(1, dw-1))
	yr := float64(sh-1) / float64(max(1, dh-1))
	for y := 0; y < dh; y++ {
		sy := float64(y) * yr
		y0 := int(sy)
		y1 := min(y0+1, sh-1)
		wy := sy - float64(y0)
		for x := 0; x < dw; x++ {
			sx := float64(x) * xr
			x0 := int(sx)
			x1 := min(x0+1, sw-1)
			wx := sx - float64(x0)
			c00 := color.RGBAModel.Convert(src.At(sb.Min.X+x0, sb.Min.Y+y0)).(color.RGBA)
			c10 := color.RGBAModel.Convert(src.At(sb.Min.X+x1, sb.Min.Y+y0)).(color.RGBA)
			c01 := color.RGBAModel.Convert(src.At(sb.Min.X+x0, sb.Min.Y+y1)).(color.RGBA)
			c11 := color.RGBAModel.Convert(src.At(sb.Min.X+x1, sb.Min.Y+y1)).(color.RGBA)
			dst.SetRGBA(x, y, color.RGBA{
				R: lerp(c00.R, c10.R, c01.R, c11.R, wx, wy),
				G: lerp(c00.G, c10.G, c01.G, c11.G, wx, wy),
				B: lerp(c00.B, c10.B, c01.B, c11.B, wx, wy),
				A: lerp(c00.A, c10.A, c01.A, c11.A, wx, wy),
			})
		}
	}
	return dst
}

func lerp(c00, c10, c01, c11 uint8, wx, wy float64) uint8 {
	top := float64(c00)*(1-wx) + float64(c10)*wx
	bot := float64(c01)*(1-wx) + float64(c11)*wx
	v := top*(1-wy) + bot*wy
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(v + 0.5)
}
