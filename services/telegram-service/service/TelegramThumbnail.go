package service

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"

	"golang.org/x/image/draw"
)

const (
	// Telegram accepts a document thumbnail only as a JPEG of at most
	// 320x320 px and 200 kB; anything else is ignored or rejected.
	telegramThumbMaxSide  = 320
	telegramThumbMaxBytes = 200 << 10
)

// telegramThumbQualities are tried in order until the JPEG fits the size limit.
var telegramThumbQualities = []int{87, 75, 60, 45}

// prepareTelegramThumbnail converts an image to a JPEG that satisfies the
// Telegram thumbnail limits, scaling it down to fit 320x320 if needed.
func prepareTelegramThumbnail(data []byte) ([]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode thumbnail: %w", err)
	}

	img := src
	bounds := src.Bounds()
	if w, h := bounds.Dx(), bounds.Dy(); w > telegramThumbMaxSide || h > telegramThumbMaxSide {
		scaledW, scaledH := fitInside(w, h, telegramThumbMaxSide)
		dst := image.NewRGBA(image.Rect(0, 0, scaledW, scaledH))
		draw.CatmullRom.Scale(dst, dst.Bounds(), src, bounds, draw.Src, nil)
		img = dst
	}

	var buf bytes.Buffer
	for _, quality := range telegramThumbQualities {
		buf.Reset()
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
			return nil, fmt.Errorf("encode thumbnail: %w", err)
		}
		if buf.Len() <= telegramThumbMaxBytes {
			return buf.Bytes(), nil
		}
	}
	return nil, fmt.Errorf("thumbnail is %d bytes at lowest quality, limit is %d", buf.Len(), telegramThumbMaxBytes)
}

// fitInside scales w x h down to fit a maxSide square, keeping the aspect ratio.
func fitInside(w, h, maxSide int) (int, int) {
	if w >= h {
		return maxSide, max(1, h*maxSide/w)
	}
	return max(1, w*maxSide/h), maxSide
}
