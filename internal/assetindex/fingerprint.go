package assetindex

import (
	"hash/crc32"
	"io"
	"os"
	"strconv"
)

// Content fingerprints give an asset a stable identity for tagging that survives a
// resync and travels across machines (see Asset.Fingerprint). Byte-identical files
// share one, so a tag set on a file bundled across packs applies to every copy.
//
// Zip and loose files use CRC32 of the content plus the byte size: the CRC is read
// for free from a zip's central directory and computed in one pass for a loose
// file. Unity-package entries use the package's stable GUID (free during
// enumeration, and preserved by Unity across re-exports). CRC32 is not
// cryptographic; paired with the exact size a collision between two genuinely
// distinct files in one library is negligible, and the only cost of one would be a
// spurious shared tag.

func crcFingerprint(crc uint32, size int64) string {
	return "crc32:" + strconv.FormatUint(uint64(crc), 16) + ":" + strconv.FormatInt(size, 10)
}

func unityFingerprint(guid string) string {
	return "uguid:" + guid
}

// looseFingerprint reads a loose file once to CRC32 its bytes. It returns "" (a
// non-taggable asset) rather than an error when the file cannot be read, so a
// single unreadable file never fails a whole scan.
func looseFingerprint(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := crc32.NewIEEE()
	n, err := io.Copy(h, f)
	if err != nil {
		return ""
	}
	return crcFingerprint(h.Sum32(), n)
}
