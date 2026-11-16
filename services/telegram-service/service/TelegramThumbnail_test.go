package service

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"testing"
)

func encodeTestJpeg(t *testing.T, w, h int, noisy bool) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rnd := rand.New(rand.NewPCG(1, 2))
	for y := range h {
		for x := range w {
			c := color.RGBA{R: uint8(x), G: uint8(y), B: 128, A: 255}
			if noisy {
				c = color.RGBA{R: uint8(rnd.IntN(256)), G: uint8(rnd.IntN(256)), B: uint8(rnd.IntN(256)), A: 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decodeThumb(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if format != "jpeg" {
		t.Fatalf("format = %s, want jpeg", format)
	}
	return img
}

func TestPrepareTelegramThumbnail_ScalesDownKeepingAspect(t *testing.T) {
	out, err := prepareTelegramThumbnail(encodeTestJpeg(t, 360, 640, false))
	if err != nil {
		t.Fatal(err)
	}

	bounds := decodeThumb(t, out).Bounds()
	if bounds.Dx() != 180 || bounds.Dy() != 320 {
		t.Errorf("size = %dx%d, want 180x320", bounds.Dx(), bounds.Dy())
	}
}

func TestPrepareTelegramThumbnail_KeepsSmallImageSize(t *testing.T) {
	out, err := prepareTelegramThumbnail(encodeTestJpeg(t, 100, 80, false))
	if err != nil {
		t.Fatal(err)
	}

	bounds := decodeThumb(t, out).Bounds()
	if bounds.Dx() != 100 || bounds.Dy() != 80 {
		t.Errorf("size = %dx%d, want 100x80", bounds.Dx(), bounds.Dy())
	}
}

func TestPrepareTelegramThumbnail_FitsByteLimit(t *testing.T) {
	// Random noise compresses badly, forcing the lower quality steps.
	out, err := prepareTelegramThumbnail(encodeTestJpeg(t, 2000, 2000, true))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > telegramThumbMaxBytes {
		t.Errorf("thumbnail is %d bytes, limit %d", len(out), telegramThumbMaxBytes)
	}
}

func TestPrepareTelegramThumbnail_ConvertsPngToJpeg(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 50, 50))); err != nil {
		t.Fatal(err)
	}

	out, err := prepareTelegramThumbnail(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	decodeThumb(t, out)
}

func TestPrepareTelegramThumbnail_RejectsInvalidData(t *testing.T) {
	if _, err := prepareTelegramThumbnail([]byte("not an image")); err == nil {
		t.Error("expected error for invalid image data")
	}
}

func TestFitInside(t *testing.T) {
	for _, tc := range []struct{ w, h, wantW, wantH int }{
		{w: 640, h: 360, wantW: 320, wantH: 180},
		{w: 360, h: 640, wantW: 180, wantH: 320},
		{w: 500, h: 500, wantW: 320, wantH: 320},
		{w: 10000, h: 1, wantW: 320, wantH: 1},
	} {
		if w, h := fitInside(tc.w, tc.h, 320); w != tc.wantW || h != tc.wantH {
			t.Errorf("fitInside(%d, %d) = %dx%d, want %dx%d", tc.w, tc.h, w, h, tc.wantW, tc.wantH)
		}
	}
}
