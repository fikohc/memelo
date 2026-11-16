package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/fikohc/memelo/common/helper"
)

// MediaSizeProber returns the size of media behind a URL without downloading it.
type MediaSizeProber interface {
	Size(ctx context.Context, url string) (int64, error)
}

type MediaSizeProberImpl struct {
	log *slog.Logger
}

func (p *MediaSizeProberImpl) Size(ctx context.Context, url string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return 0, fmt.Errorf("MediaSizeProber: create request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("MediaSizeProber: head request: %w", err)
	}
	defer helper.QuietClose(resp.Body, p.log)

	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("MediaSizeProber: non-2xx status: %d", resp.StatusCode)
	}
	if resp.ContentLength <= 0 {
		return 0, errors.New("MediaSizeProber: unknown content length")
	}
	return resp.ContentLength, nil
}

func NewMediaSizeProber() MediaSizeProber {
	return &MediaSizeProberImpl{log: slog.With("service", "MediaSizeProber")}
}
