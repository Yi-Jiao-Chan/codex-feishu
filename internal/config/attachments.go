package config

import (
	"errors"
	"path/filepath"
)

// Zero limits select the documented defaults. Existing installations remain
// disabled until their administrator opts into the additional receive scopes.
type AttachmentConfig struct {
	Enabled           bool   `toml:"enabled" json:"enabled"`
	ReceiveStandalone bool   `toml:"receive_standalone" json:"receive_standalone"`
	Directory         string `toml:"directory" json:"directory"`
	MaxResources      int    `toml:"max_resources" json:"max_resources"`
	MaxImages         int    `toml:"max_images" json:"max_images"`
	MaxImageBytes     int64  `toml:"max_image_bytes" json:"max_image_bytes"`
	MaxImagesBytes    int64  `toml:"max_images_bytes" json:"max_images_bytes"`
	MaxFileBytes      int64  `toml:"max_file_bytes" json:"max_file_bytes"`
	MaxBatchBytes     int64  `toml:"max_batch_bytes" json:"max_batch_bytes"`
	MaxImagePixels    int64  `toml:"max_image_pixels" json:"max_image_pixels"`
	DiskQuotaBytes    int64  `toml:"disk_quota_bytes" json:"disk_quota_bytes"`
	DownloadWorkers   int    `toml:"download_workers" json:"download_workers"`
}

func (c Config) AttachmentLimits() AttachmentConfig {
	a := c.Attachments
	if a.Directory == "" {
		a.Directory = filepath.Join(filepath.Dir(c.Storage.DatabaseFile), "attachments")
	}
	if a.MaxResources == 0 {
		a.MaxResources = 10
	}
	if a.MaxImages == 0 {
		a.MaxImages = 5
	}
	if a.MaxImageBytes == 0 {
		a.MaxImageBytes = 10 << 20
	}
	if a.MaxImagesBytes == 0 {
		a.MaxImagesBytes = 20 << 20
	}
	if a.MaxFileBytes == 0 {
		a.MaxFileBytes = 20 << 20
	}
	if a.MaxBatchBytes == 0 {
		a.MaxBatchBytes = 50 << 20
	}
	if a.MaxImagePixels == 0 {
		a.MaxImagePixels = 40_000_000
	}
	if a.DiskQuotaBytes == 0 {
		a.DiskQuotaBytes = 1 << 30
	}
	if a.DownloadWorkers == 0 {
		a.DownloadWorkers = 2
	}
	return a
}

func (c Config) ValidateAttachments() error {
	a := c.AttachmentLimits()
	if c.Attachments.ReceiveStandalone && !c.Attachments.Enabled {
		return errors.New("standalone attachments require attachments.enabled")
	}
	if c.Attachments.Directory != "" && !filepath.IsAbs(c.Attachments.Directory) {
		return errors.New("attachments.directory must be absolute")
	}
	if a.MaxResources < 1 || a.MaxResources > 20 || a.MaxImages < 1 || a.MaxImages > a.MaxResources || a.DownloadWorkers < 1 || a.DownloadWorkers > 8 ||
		a.MaxImageBytes < 1 || a.MaxImagesBytes < 1 || a.MaxFileBytes < 1 || a.MaxBatchBytes < 1 || a.MaxImagePixels < 1 || a.MaxImagePixels > 100_000_000 || a.DiskQuotaBytes < 1 ||
		a.MaxImageBytes > 10<<20 || a.MaxImagesBytes > 20<<20 || a.MaxFileBytes > 100<<20 || a.MaxBatchBytes > 500<<20 || a.MaxImagesBytes > a.MaxBatchBytes {
		return errors.New("invalid attachment resource, byte, pixel or worker limits")
	}
	return nil
}
