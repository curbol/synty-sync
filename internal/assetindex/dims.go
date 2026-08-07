package assetindex

import (
	"bytes"
	"encoding/binary"
	"image"

	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

// dimsHeadBytes is how much of an image's leading bytes are read to recover its
// pixel dimensions. The header carrying width/height sits at the front of every
// format handled here (a JPEG's SOF marker can trail large embedded EXIF, which is
// why this is generous rather than a few dozen bytes).
const dimsHeadBytes = 8 << 10

// imageDims recovers an image's pixel dimensions from its leading bytes, or 0,0
// when ext isn't a raster format handled here or the header can't be parsed. png,
// jpeg and gif go through the stdlib decoders; tga, psd and bmp have no stdlib
// support (and tga has no magic number to sniff), so their fixed-offset headers are
// read directly.
func imageDims(head []byte, ext string) (int, int) {
	switch ext {
	case "png", "jpg", "jpeg", "gif":
		cfg, _, err := image.DecodeConfig(bytes.NewReader(head))
		if err != nil {
			return 0, 0
		}
		return cfg.Width, cfg.Height
	case "tga":
		if len(head) < 18 {
			return 0, 0
		}
		return int(binary.LittleEndian.Uint16(head[12:])), int(binary.LittleEndian.Uint16(head[14:]))
	case "psd":
		if len(head) < 26 || string(head[:4]) != "8BPS" {
			return 0, 0
		}
		return int(binary.BigEndian.Uint32(head[18:])), int(binary.BigEndian.Uint32(head[14:]))
	case "bmp":
		if len(head) < 26 || string(head[:2]) != "BM" {
			return 0, 0
		}
		return int(int32(binary.LittleEndian.Uint32(head[18:]))), int(int32(binary.LittleEndian.Uint32(head[22:])))
	}
	return 0, 0
}

// isDimExt reports whether an extension is a raster format imageDims can measure.
func isDimExt(ext string) bool {
	switch ext {
	case "png", "jpg", "jpeg", "gif", "tga", "psd", "bmp":
		return true
	}
	return false
}
