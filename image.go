package livechat

import (
	"bytes"
	"image"
	_ "image/gif"  // register decoders for DecodeConfig
	_ "image/jpeg" //
	_ "image/png"  //
	"net/http"

	_ "golang.org/x/image/webp" //
)

// imageExts are the formats accepted in both directions: what browsers
// display and 飞书 takes as a message image.
var imageExts = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
	"image/gif":  "gif",
	"image/webp": "webp",
}

// inspectImage sniffs the format from the bytes (never trusting a file name
// or a declared content type) and reads the dimensions.
func inspectImage(data []byte) (img Image, ext string, err error) {
	ct := http.DetectContentType(data)
	ext, ok := imageExts[ct]
	if !ok {
		return Image{}, "", ErrBadImage
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Image{}, "", ErrBadImage
	}
	return Image{ContentType: ct, Width: cfg.Width, Height: cfg.Height}, ext, nil
}
