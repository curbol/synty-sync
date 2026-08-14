package assetindex

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

const (
	glbMagic     = 0x46546C67 // "glTF"
	glbChunkJSON = 0x4E4F534A // "JSON"
	// maxGLBJSON caps the JSON chunk read so a corrupt length can't drive a huge
	// allocation; real glTF scene descriptions are well under this.
	maxGLBJSON = 128 << 20
)

// glbAnimationNames reads only the JSON chunk of a .glb (binary glTF) and returns
// its animation names in file order — not the (potentially large) binary buffer.
// It returns a nil slice for a glTF with no animations, and an error only when the
// file is not a readable GLB.
func glbAnimationNames(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var head [20]byte // 12-byte GLB header + 8-byte first chunk header
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return nil, err
	}
	if binary.LittleEndian.Uint32(head[0:]) != glbMagic {
		return nil, fmt.Errorf("not a glb: %s", path)
	}
	chunkLen := binary.LittleEndian.Uint32(head[12:])
	if binary.LittleEndian.Uint32(head[16:]) != glbChunkJSON {
		return nil, fmt.Errorf("first glb chunk is not JSON: %s", path)
	}
	if chunkLen > maxGLBJSON {
		return nil, fmt.Errorf("glb JSON chunk too large (%d bytes): %s", chunkLen, path)
	}
	jb := make([]byte, chunkLen)
	if _, err := io.ReadFull(f, jb); err != nil {
		return nil, err
	}
	var doc struct {
		Animations []struct {
			Name string `json:"name"`
		} `json:"animations"`
	}
	if err := json.Unmarshal(jb, &doc); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(doc.Animations))
	for _, a := range doc.Animations {
		names = append(names, a.Name)
	}
	return names, nil
}
