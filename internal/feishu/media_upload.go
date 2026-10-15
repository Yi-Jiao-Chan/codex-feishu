package feishu

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"unicode"

	core "github.com/larksuite/oapi-sdk-go/v3/core"
)

const maxUploadImageBytes = 10 << 20

// UploadImage uploads supplied bytes for message use. It does not read paths or
// fetch URLs; callers must journal the operation before invoking it.
func (g *Gateway) UploadImage(ctx context.Context, name string, data []byte) (string, error) {
	if len(data) == 0 || len(data) > maxUploadImageBytes {
		return "", errors.New("image upload requires 1 byte to 10 MiB")
	}
	if strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", errors.New("image upload filename contains control characters")
	}
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "" || name == "." || name == ".." || name == "/" {
		return "", errors.New("image upload filename absent")
	}
	var r struct {
		Data struct {
			ImageKey string `json:"image_key"`
		} `json:"data"`
	}
	body := core.NewFormdata().AddField("image_type", "message").AddFileWithName("image", name, bytes.NewReader(data))
	if err := g.request(ctx, "POST", "/open-apis/im/v1/images", nil, body, &r); err != nil {
		return "", err
	}
	if r.Data.ImageKey == "" {
		return "", &Unknown{"upload image response missing image_key"}
	}
	return r.Data.ImageKey, nil
}
