// Package lockfile reads and writes the committed record of owned packs and the
// versions currently mirrored. encoding/json sorts map keys, so MarshalIndent
// yields a stable, minimally-diffing file across runs.
package lockfile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Lockfile is the on-disk record. customerId is deliberately absent (account PII).
type Lockfile struct {
	GeneratedAt string          `json:"generatedAt"`
	Packs       map[string]Pack `json:"packs"`
}

// Pack keys by display-name slug; files key by "fileToken|variant".
type Pack struct {
	DisplayName string          `json:"displayName"`
	OrderID     int             `json:"orderId"`
	OrderItemID int             `json:"orderItemId"`
	Files       map[string]File `json:"files"`
}

// File records one owned file. A filtered-out (not downloaded) file has
// Tracked=false and no SHA256/CachePath. A shared bundled file points multiple
// packs' entries at the same CachePath.
//
// The two size fields are deliberately separate. AdvertisedSize is what the portal's
// label said and refreshes on every run; SizeBytes is what actually landed on disk
// and is written only when a run resolves the file. Folding them into one field
// leaves no way to tell a rounded display figure from the count the integrity check
// compares against.
type File struct {
	FileToken      string `json:"fileToken"`
	Variant        string `json:"variant"`
	Version        string `json:"version"`
	FileID         int    `json:"fileId"`
	Tracked        bool   `json:"tracked"`
	SHA256         string `json:"sha256,omitempty"`
	AdvertisedSize int64  `json:"advertisedSize,omitempty"`
	SizeBytes      int64  `json:"sizeBytes,omitempty"`
	CachePath      string `json:"cachePath,omitempty"`
	DownloadedAt   string `json:"downloadedAt,omitempty"`
}

// New returns an empty lockfile ready to populate.
func New() Lockfile { return Lockfile{Packs: map[string]Pack{}} }

// Load reads a lockfile, returning an empty one if the path does not exist.
func Load(path string) (Lockfile, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return New(), nil
	}
	if err != nil {
		return Lockfile{}, fmt.Errorf("read lockfile: %w", err)
	}
	var lf Lockfile
	if err := json.Unmarshal(raw, &lf); err != nil {
		return Lockfile{}, fmt.Errorf("parse lockfile: %w", err)
	}
	if lf.Packs == nil {
		lf.Packs = map[string]Pack{}
	}
	return lf, nil
}

// Save writes the lockfile atomically with sorted keys and a trailing newline.
func Save(path string, lf Lockfile) error {
	raw, err := json.MarshalIndent(lf, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".synty-lock-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}
