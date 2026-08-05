package assetindex

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// unityEntry accumulates the members of one GUID directory as the tar streams by.
type unityEntry struct {
	pathname   string
	hasAsset   bool
	assetSize  int64
	hasPreview bool
}

// splitUnityName splits a tar member name into its GUID dir and member, tolerating
// a leading "./". It returns ok=false for names that aren't a two-part
// <guid>/<member> or whose guid is unsafe.
func splitUnityName(name string) (guid, member string, ok bool) {
	name = strings.TrimPrefix(name, "./")
	parts := strings.SplitN(name, "/", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	guid, member = parts[0], parts[1]
	if guid == "" || guid == ".." || strings.ContainsAny(guid, `/\`) {
		return "", "", false
	}
	return guid, member, true
}

// unityAssets enumerates the payload-bearing entries of a .unitypackage. It streams
// the gzip+tar once, resolving each GUID's real path from its `pathname` member and
// noting an optional `preview.png`. GUIDs with no `asset` payload (Unity directory
// placeholders) are dropped so they never become phantom index rows.
func unityAssets(archivePath, displayRel, vendor, pack, variant string) ([]Asset, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("gzip %s: %w", filepath.Base(archivePath), err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	entries := map[string]*unityEntry{}
	var order []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar %s: %w", filepath.Base(archivePath), err)
		}
		guid, member, ok := splitUnityName(hdr.Name)
		if !ok {
			continue
		}
		e := entries[guid]
		if e == nil {
			e = &unityEntry{}
			entries[guid] = e
			order = append(order, guid)
		}
		switch member {
		case "asset":
			e.hasAsset = true
			e.assetSize = hdr.Size
		case "pathname":
			b, err := io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			e.pathname = strings.TrimSpace(firstLine(string(b)))
		case "preview.png":
			e.hasPreview = true
		}
	}

	var assets []Asset
	for _, guid := range order {
		e := entries[guid]
		if !e.hasAsset || e.pathname == "" || !safeEntry(e.pathname) || skipEntry(e.pathname) {
			continue
		}
		src := Source{Kind: SourceUnityPackage, ArchivePath: archivePath, Guid: guid, Pathname: e.pathname, HasPreview: e.hasPreview}
		assets = append(assets, newAsset(src,
			path.Base(e.pathname),
			archiveRel(displayRel, e.pathname),
			vendor, pack, variant,
			e.assetSize,
		))
	}
	return assets, nil
}

// extractUnityPackage decompresses a .unitypackage once, writing each GUID's
// `asset` and `preview.png` payloads to <destDir>/<guid>/. Metadata members
// (asset.meta, pathname) are not written. destDir is expected to be a temp dir the
// caller renames into place atomically.
func extractUnityPackage(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		guid, member, ok := splitUnityName(hdr.Name)
		if !ok || (member != "asset" && member != "preview.png") {
			continue
		}
		dir := filepath.Join(destDir, guid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		out, err := os.Create(filepath.Join(dir, member))
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
