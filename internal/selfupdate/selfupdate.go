// Package selfupdate implements `synty-sync update`: it fetches a release from the
// GitHub API, downloads the binary for the current platform, and atomically replaces
// the running executable. The repo is private, so downloads use a token resolved
// from GITHUB_TOKEN / GH_TOKEN / the gh CLI, hitting the asset API URL with an
// octet-stream Accept header (the same model as the install script).
package selfupdate

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	releasesAPI = "https://api.github.com/repos/curbol/synty-sync/releases"
	binaryName  = "synty-sync"
)

type release struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"assets"`
}

// Run updates the binary to target (a version like "0.2.0"), or to the latest
// release when target is empty. current is the running binary's version.
func Run(current, target string) error {
	current = strings.TrimSpace(current)
	if current == "" || current == "dev" {
		return fmt.Errorf("this is a dev build (version %q); `update` only works on release builds — install one with install.sh", current)
	}
	token := resolveToken()

	rel, err := fetchRelease(token, target)
	if err != nil {
		return err
	}
	relVer := strings.TrimPrefix(rel.TagName, "v")
	if relVer == strings.TrimPrefix(current, "v") {
		label := "latest"
		if target != "" {
			label = "requested"
		}
		fmt.Fprintf(os.Stderr, "already on the %s version (%s)\n", label, relVer)
		return nil
	}

	assetURL, err := platformAsset(rel)
	if err != nil {
		return err
	}
	if err := downloadAndReplace(token, assetURL); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "updated %s to version %s\n", binaryName, relVer)
	return nil
}

// resolveToken finds a GitHub token: env first, then the gh CLI. Empty is allowed
// (public assets), but this repo is private so a token is normally required.
func resolveToken() string {
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		return t
	}
	if t := os.Getenv("GH_TOKEN"); t != "" {
		return t
	}
	gh, err := exec.LookPath("gh")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, gh, "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func newRequest(token, method, url string) (*http.Request, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	return req, nil
}

func fetchRelease(token, target string) (*release, error) {
	url := releasesAPI + "/latest"
	if target != "" {
		tag := target
		if !strings.HasPrefix(tag, "v") {
			tag = "v" + tag
		}
		url = releasesAPI + "/tags/" + tag
	}
	req, err := newRequest(token, http.MethodGet, url)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		hint := ""
		if token == "" {
			hint = " (no GitHub token found; set GITHUB_TOKEN or run `gh auth login`)"
		}
		if target != "" {
			return nil, fmt.Errorf("version %s not found%s", target, hint)
		}
		return nil, fmt.Errorf("no releases found%s", hint)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GitHub API %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var r release
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("parsing release: %w", err)
	}
	return &r, nil
}

// platformAsset returns the asset API URL for the current OS/arch, matching the
// label suffix the release workflow uses.
func platformAsset(rel *release) (string, error) {
	var suffix string
	switch runtime.GOOS {
	case "darwin":
		suffix = "mac-intel.zip"
		if runtime.GOARCH == "arm64" {
			suffix = "mac-apple.zip"
		}
	case "linux":
		suffix = "linux-intel.zip"
		if runtime.GOARCH == "arm64" {
			suffix = "linux-arm64.zip"
		}
	case "windows":
		suffix = "win.zip"
	default:
		return "", fmt.Errorf("unsupported platform %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	for _, a := range rel.Assets {
		if strings.HasSuffix(a.Name, suffix) {
			return a.URL, nil
		}
	}
	names := make([]string, len(rel.Assets))
	for i, a := range rel.Assets {
		names[i] = a.Name
	}
	return "", fmt.Errorf("no asset matching %s; available: %v", suffix, names)
}

func downloadAndReplace(token, assetURL string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating current binary: %w", err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return fmt.Errorf("resolving binary path: %w", err)
	}

	// Stage next to the target binary so the final rename stays on one filesystem
	// (a temp dir under /tmp is often a separate device, and rename can't cross it).
	tmp, err := os.MkdirTemp(filepath.Dir(exe), ".synty-sync-update-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	zipPath := filepath.Join(tmp, "download.zip")
	fmt.Fprintln(os.Stderr, "downloading update…")
	if err := download(token, assetURL, zipPath); err != nil {
		return err
	}
	binPath, err := extractBinary(zipPath, tmp)
	if err != nil {
		return err
	}
	if err := os.Chmod(binPath, 0o755); err != nil {
		return err
	}

	// Back up the current binary, then swap the new one in; restore on failure.
	backup := exe + ".backup"
	if err := copyFile(exe, backup); err != nil {
		return fmt.Errorf("creating backup: %w", err)
	}
	defer os.Remove(backup)
	if err := os.Rename(binPath, exe); err != nil {
		// Rename should stay on one device now, but fall back to an in-place copy
		// for any exotic mount (overlay, bind) where it still can't.
		if cerr := copyFile(binPath, exe); cerr != nil {
			if restoreErr := copyFile(backup, exe); restoreErr != nil {
				return fmt.Errorf("install failed: %w; restore also failed: %w", err, restoreErr)
			}
			return fmt.Errorf("install failed (original restored): %w", err)
		}
	}
	return nil
}

func download(token, url, dst string) error {
	req, err := newRequest(token, http.MethodGet, url)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return fmt.Errorf("downloading: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func extractBinary(zipPath, dir string) (string, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", fmt.Errorf("opening zip: %w", err)
	}
	defer r.Close()
	want := binaryName
	if runtime.GOOS == "windows" {
		want = binaryName + ".exe"
	}
	for _, f := range r.File {
		if f.Name != want {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		defer rc.Close()
		out := filepath.Join(dir, want)
		w, err := os.Create(out)
		if err != nil {
			return "", err
		}
		defer w.Close()
		if _, err := io.Copy(w, rc); err != nil {
			return "", err
		}
		return out, nil
	}
	return "", fmt.Errorf("binary %q not found in release archive", want)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
