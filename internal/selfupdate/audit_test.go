package selfupdate

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeBinary is bytes that pass the executable sniff on the running platform, so a
// test can exercise the install path without shipping a real binary.
func fakeBinary(tag string) []byte {
	var magic []byte
	switch runtime.GOOS {
	case "darwin":
		magic = []byte{0xcf, 0xfa, 0xed, 0xfe}
	case "windows":
		magic = []byte("MZ")
	default:
		magic = []byte("\x7fELF")
	}
	return append(magic, []byte(tag)...)
}

func zipWith(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func assetServer(t *testing.T, status int, body []byte) (*httptest.Server, *http.Header) {
	t.Helper()
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func installedBinaryName() string {
	if runtime.GOOS == "windows" {
		return binaryName + ".exe"
	}
	return binaryName
}

// The running binary is replaced by renaming a fully-written file into place, so a
// failure never leaves a half-written executable and nothing is left in its dir.
func TestInstallReplacesBinaryInPlace(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "synty-sync")
	if err := os.WriteFile(exe, fakeBinary("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv, hdr := assetServer(t, http.StatusOK, zipWith(t, installedBinaryName(), fakeBinary("NEW")))

	if err := installTo(context.Background(), "tok", srv.URL, exe); err != nil {
		t.Fatalf("installTo: %v", err)
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fakeBinary("NEW")) {
		t.Errorf("binary content = %q, want the downloaded one", got)
	}
	fi, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755", fi.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("update left %v behind, want only the binary", names)
	}
	if auth := hdr.Get("Authorization"); auth != "token tok" {
		t.Errorf("Authorization = %q", auth)
	}
	if acc := hdr.Get("Accept"); acc != "application/octet-stream" {
		t.Errorf("Accept = %q", acc)
	}
}

// A release asset that is not an executable (an error page, the wrong file) must be
// refused rather than swapped over a working binary.
func TestInstallRejectsNonExecutableAsset(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "synty-sync")
	if err := os.WriteFile(exe, fakeBinary("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv, _ := assetServer(t, http.StatusOK, zipWith(t, installedBinaryName(), []byte("<html>not found</html>")))

	if err := installTo(context.Background(), "tok", srv.URL, exe); err == nil {
		t.Fatal("expected the non-executable asset to be refused")
	}
	got, _ := os.ReadFile(exe)
	if !bytes.Equal(got, fakeBinary("OLD")) {
		t.Error("the working binary was replaced anyway")
	}
}

func TestInstallLeavesBinaryOnFailedDownload(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "synty-sync")
	if err := os.WriteFile(exe, fakeBinary("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv, _ := assetServer(t, http.StatusForbidden, nil)

	if err := installTo(context.Background(), "tok", srv.URL, exe); err == nil {
		t.Fatal("expected the failed download to surface")
	}
	got, _ := os.ReadFile(exe)
	if !bytes.Equal(got, fakeBinary("OLD")) {
		t.Error("the working binary was disturbed by a failed download")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("staging left %d files behind", len(entries))
	}
}

// A release archive without the expected binary must not silently install nothing.
func TestExtractBinaryRequiresTheNamedBinary(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "download.zip")
	if err := os.WriteFile(zipPath, zipWith(t, "README.md", []byte("hi")), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := extractBinary(zipPath, dir); err == nil {
		t.Error("expected an error when the archive has no binary")
	}
}

func TestResolveTokenPrefersGithubToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "primary")
	t.Setenv("GH_TOKEN", "secondary")
	if got := resolveToken(context.Background()); got != "primary" {
		t.Errorf("resolveToken = %q, want GITHUB_TOKEN to win", got)
	}
	t.Setenv("GITHUB_TOKEN", "")
	if got := resolveToken(context.Background()); got != "secondary" {
		t.Errorf("resolveToken = %q, want the GH_TOKEN fallback", got)
	}
}

// The API error body is echoed to the user; the token must not travel with it.
func TestFetchReleaseErrorOmitsToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"message":"Bad credentials"}`)
	}))
	defer srv.Close()

	old := releasesAPIURL
	releasesAPIURL = srv.URL
	defer func() { releasesAPIURL = old }()

	_, err := fetchRelease(context.Background(), "s3cr3t-token", "")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "s3cr3t-token") {
		t.Errorf("token leaked into the error: %q", err)
	}
}

// replaceBinary has to work when the target is the image currently executing.
// Windows refuses os.Rename onto a running .exe but does allow renaming it aside,
// which is why the current binary is moved out of the way first. The observable
// contract on every platform: the new bytes land, and no staging file survives.
func TestReplaceBinaryLeavesNoResidue(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "synty-sync")
	if err := os.WriteFile(exe, fakeBinary("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(dir, "staged")
	if err := os.WriteFile(staged, fakeBinary("NEW"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := replaceBinary(staged, exe); err != nil {
		t.Fatalf("replaceBinary: %v", err)
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fakeBinary("NEW")) {
		t.Errorf("binary content = %q, want the new one", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "synty-sync" {
			t.Errorf("left %q behind", e.Name())
		}
	}
}

// A leftover .old from an interrupted update must not block the next one.
func TestReplaceBinaryClearsAStaleAside(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "synty-sync")
	if err := os.WriteFile(exe, fakeBinary("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe+".old", fakeBinary("ANCIENT"), 0o755); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(dir, "staged")
	if err := os.WriteFile(staged, fakeBinary("NEW"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := replaceBinary(staged, exe); err != nil {
		t.Fatalf("replaceBinary with a stale .old: %v", err)
	}
	got, _ := os.ReadFile(exe)
	if !bytes.Equal(got, fakeBinary("NEW")) {
		t.Errorf("binary content = %q, want the new one", got)
	}
}

// The gh CLI is the last tier of token resolution and the one most likely to break,
// and it was the only tier with no coverage. A stub gh on PATH exercises both the
// success path and the "gh present but not logged in" path.
func TestResolveTokenFallsBackToGhCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stub is POSIX-only")
	}
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")

	stubDir := t.TempDir()
	writeGh := func(script string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(stubDir, "gh"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", stubDir)

	writeGh("#!/bin/sh\necho gh-cli-token\n")
	if got := resolveToken(context.Background()); got != "gh-cli-token" {
		t.Errorf("token = %q, want the gh CLI's", got)
	}

	// Not logged in: gh exits non-zero, and the caller must get "" rather than gh's
	// error text masquerading as a token.
	writeGh("#!/bin/sh\necho 'not logged in' >&2\nexit 1\n")
	if got := resolveToken(context.Background()); got != "" {
		t.Errorf("token = %q, want empty when gh fails", got)
	}

	// No gh at all.
	t.Setenv("PATH", t.TempDir())
	if got := resolveToken(context.Background()); got != "" {
		t.Errorf("token = %q, want empty with no gh on PATH", got)
	}
}

// The env tiers must still win over the CLI.
func TestResolveTokenPrefersEnvOverGhCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stub is POSIX-only")
	}
	stubDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stubDir, "gh"), []byte("#!/bin/sh\necho gh-cli-token\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir)
	t.Setenv("GH_TOKEN", "from-gh-token")
	t.Setenv("GITHUB_TOKEN", "")
	if got := resolveToken(context.Background()); got != "from-gh-token" {
		t.Errorf("token = %q, want GH_TOKEN to beat the CLI", got)
	}
	t.Setenv("GITHUB_TOKEN", "from-github-token")
	if got := resolveToken(context.Background()); got != "from-github-token" {
		t.Errorf("token = %q, want GITHUB_TOKEN to win outright", got)
	}
}

// The rename fallback exists for a cross-device or otherwise exotic mount, and
// nothing reached it before: installTo always stages beside the target, so the first
// rename always succeeds and the copy path was never exercised.
func TestReplaceBinaryFallsBackToCopyingAcrossDevices(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "synty-sync")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	newPath := filepath.Join(otherDevice(t, dir), "new")
	if err := os.WriteFile(newPath, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(newPath) })

	if err := replaceBinary(newPath, exe); err != nil {
		t.Fatalf("replaceBinary across devices: %v", err)
	}
	got, err := os.ReadFile(exe)
	if err != nil || string(got) != "new" {
		t.Fatalf("exe = %q (%v), want the new binary", got, err)
	}
	info, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want the copy to keep 0755", info.Mode().Perm())
	}
	if _, err := os.Stat(exe + ".old"); !os.IsNotExist(err) {
		t.Error("the aside copy was left behind")
	}
}

// otherDevice returns a writable directory a rename into dir actually fails from,
// which is the only way to reach os.Rename's fallback. It probes rather than
// comparing device numbers so it stays portable and tests the property directly.
func otherDevice(t *testing.T, dir string) string {
	t.Helper()
	for _, candidate := range []string{"/dev/shm", os.TempDir()} {
		f, err := os.CreateTemp(candidate, "synty-xdev-")
		if err != nil {
			continue
		}
		probe := f.Name()
		f.Close()
		landing := filepath.Join(dir, "probe")
		if err := os.Rename(probe, landing); err != nil {
			os.Remove(probe)
			return candidate
		}
		os.Remove(landing)
	}
	t.Skip("no writable directory on a second filesystem to force a cross-device rename")
	return ""
}

// When the install fails and the original cannot be put back either, exe no longer
// exists: it was renamed aside and nothing returned it. The error has to say where
// it went, or the user is left with no binary and no idea there is one to recover.
func TestReplaceBinaryNamesTheAsideCopyWhenRestoreFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this test relies on")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "synty-sync")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A source that does not exist fails both the rename and the copy; a read-only
	// directory then fails the restore too.
	missing := filepath.Join(dir, "not-there")

	aside := exe + ".old"
	if err := os.Rename(exe, aside); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	err := replaceBinary(missing, exe)
	if err == nil {
		t.Fatal("replaceBinary reported success with nothing to install")
	}
	if !strings.Contains(err.Error(), aside) {
		t.Errorf("err = %v, want it to name %s, the only copy left", err, aside)
	}
}

// Run's own gates have no coverage otherwise, and they decide whether an update
// happens at all. The dev-build refusal is what makes a release that failed to stamp
// main.version unrecoverable in place, and the version comparison is what keeps
// `update` from re-installing the same binary forever: the workflow strips the "v"
// for the ldflag while the tag keeps it, so both sides have to be trimmed.
func TestRunRefusesADevBuild(t *testing.T) {
	err := Run(context.Background(), "dev", "")
	if err == nil {
		t.Fatal("a dev build was allowed to self-update")
	}
	if !strings.Contains(err.Error(), "dev build") {
		t.Errorf("error does not explain the refusal: %v", err)
	}
}

func TestRunStopsWhenAlreadyOnTheReleaseVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tag     string
		current string
		target  string
		want    string
	}{
		{"latest, tag carries the v", "v1.2.3", "1.2.3", "", "already on the latest version (1.2.3)"},
		{"explicit target", "v1.2.3", "1.2.3", "1.2.3", "already on the requested version (1.2.3)"},
		{"current carries the v too", "v1.2.3", "v1.2.3", "", "already on the latest version (1.2.3)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"tag_name":%q,"assets":[]}`, tc.tag)
			}))
			defer srv.Close()
			restoreURL := releasesAPIURL
			releasesAPIURL = srv.URL
			t.Cleanup(func() { releasesAPIURL = restoreURL })

			var out bytes.Buffer
			restore := progress
			progress = &out
			t.Cleanup(func() { progress = restore })

			// No asset in the release, so reaching the download would be an error.
			if err := Run(context.Background(), tc.current, tc.target); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := strings.TrimSpace(out.String()); got != tc.want {
				t.Errorf("said %q, want %q", got, tc.want)
			}
		})
	}
}

// A release that publishes nothing for this platform must say so and leave the
// working binary alone, rather than reaching downloadAndReplace with an empty URL.
func TestRunReportsAReleaseWithNoAssetForThisPlatform(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v9.9.9","assets":[{"name":"synty-sync-9.9.9-other.zip","url":"u"}]}`)
	}))
	defer srv.Close()
	restoreURL := releasesAPIURL
	releasesAPIURL = srv.URL
	t.Cleanup(func() { releasesAPIURL = restoreURL })

	err := Run(context.Background(), "1.0.0", "")
	if err == nil {
		t.Fatal("an update with no asset for this platform reported success")
	}
	if !strings.Contains(err.Error(), "no asset matching") {
		t.Errorf("error does not name the missing asset: %v", err)
	}
}

// "win.zip" is also a suffix of "darwin.zip", so matching without the separator would
// hand a Windows user a Mach-O binary the moment a release adds a darwin universal
// asset — and the label guard would still pass, since "darwin" is a label it built.
func TestPlatformAssetDoesNotMatchALabelItMerelyEndsWith(t *testing.T) {
	rel := &release{Assets: []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}{
		{Name: "synty-sync-1.0.0-darwin.zip", URL: "mach-o"},
		{Name: "synty-sync-1.0.0-win.zip", URL: "pe"},
	}}
	got, err := platformAsset(rel, "windows", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if got != "pe" {
		t.Errorf("windows resolved to %q, want the win asset", got)
	}
}
