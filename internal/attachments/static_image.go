package attachments

import (
	"encoding/binary"
	"os"
)

// Check container chunks rather than scanning arbitrary compressed bytes for
// animation markers. DecodeConfig alone would also accept an animated prefix.
func staticImage(f *os.File, format string, size int64) error {
	var start, header int64
	switch format {
	case "jpeg":
		return nil
	case "png":
		start, header = 8, 12
	case "webp":
		start, header = 12, 8
	}
	for offset := start; offset+header <= size; {
		var chunk [12]byte
		if _, err := f.ReadAt(chunk[:header], offset); err != nil {
			return &cacheError{"image-type"}
		}
		var length int64
		var tag string
		if format == "png" {
			length, tag = int64(binary.BigEndian.Uint32(chunk[:4])), string(chunk[4:8])
		} else {
			length, tag = int64(binary.LittleEndian.Uint32(chunk[4:8])), string(chunk[:4])
		}
		if tag == "acTL" || tag == "ANIM" || tag == "ANMF" {
			return &cacheError{"image-type"}
		}
		if format == "webp" && tag == "VP8X" {
			var flags [1]byte
			if _, err := f.ReadAt(flags[:], offset+8); err != nil || flags[0]&2 != 0 {
				return &cacheError{"image-type"}
			}
		}
		if length > size-offset-header {
			return &cacheError{"image-type"}
		}
		if format == "png" && tag == "IEND" {
			return nil
		}
		if format == "webp" {
			length += length & 1
		}
		offset += length + header
		if offset == size && format == "webp" {
			return nil
		}
	}
	return &cacheError{"image-type"}
}
