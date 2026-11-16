package service

import (
	"context"
	"fmt"
	"io"

	"github.com/gotd/td/fileid"
)

// TelegramFileDownloader downloads files received through the Bot API over
// MTProto, which has no 20 MB getFile limit.
type TelegramFileDownloader interface {
	Download(ctx context.Context, botApiFileId string, w io.Writer) error
}

type TelegramFileDownloaderImpl struct {
	mtproto MtprotoClient
}

func (d *TelegramFileDownloaderImpl) Download(ctx context.Context, botApiFileId string, w io.Writer) error {
	fid, err := fileid.DecodeFileID(botApiFileId)
	if err != nil {
		return fmt.Errorf("TelegramFileDownloader: decode file id failed: %w", err)
	}
	location, ok := fid.AsInputFileLocation()
	if !ok {
		return fmt.Errorf("TelegramFileDownloader: file id type %s has no file location", fid.Type)
	}
	if _, err := d.mtproto.Client().Download(location).Stream(ctx, w); err != nil {
		return fmt.Errorf("TelegramFileDownloader: download failed: %w", err)
	}
	return nil
}

func NewTelegramFileDownloader(mtproto MtprotoClient) TelegramFileDownloader {
	return &TelegramFileDownloaderImpl{mtproto: mtproto}
}
