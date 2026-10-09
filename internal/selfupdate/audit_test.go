package selfupdate

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/curbol/synty-sync/internal/releaseyml"
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

// The final move is a rename, and a rename cannot cross a filesystem, so the download
// has to land beside the binary it is replacing. Staging under the system temp dir
// still installs — replaceBinary falls through to a copy — so the suite passes either
// way, and the guarantee that the live path is only ever replaced whole is gone.
// Checked while the asset is in flight, because by the time installTo returns the
// staging dir has been cleaned up and there is nothing left to observe.
func TestUpdateStagesBesideTheBinaryItReplaces(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "synty-sync")
	if err := os.WriteFile(exe, fakeBinary("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Built on the test goroutine, before any handler can run: zipWith calls t.Fatal,
	// which is a runtime.Goexit, and from a server goroutine that aborts the response
	// mid-write instead of failing the test.
	body := zipWith(t, installedBinaryName(), fakeBinary("NEW"))
	// Written on the server goroutine and read on the test's, so it is atomic rather
	// than a plain bool.
	var staged atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Errorf("reading the install dir: %v", err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".synty-sync-update-") {
				staged.Store(true)
			}
		}
		w.Write(body)
	}))
	defer srv.Close()

	if err := installTo(context.Background(), "tok", srv.URL, exe); err != nil {
		t.Fatalf("installTo: %v", err)
	}
	if !staged.Load() {
		t.Errorf("the update did not stage in %s; the final move can now cross a filesystem, "+
			"which turns replacing the binary into a copy over the live path", dir)
	}
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

// An update replaces the bytes, not the permissions the user chose. Forcing 0755 handed
// group and other read and execute back to an install someone had locked down with
// chmod 700 on a shared machine, and reported success. The owner's execute bit is the
// one thing kept regardless, because a binary that cannot run is the one thing this must
// never leave at the install path.
func TestUpdateKeepsTheModeOfTheBinaryItReplaces(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows reports 0666 for every writable file; there are no mode bits to keep")
	}
	for _, tc := range []struct {
		name       string
		have, want os.FileMode
	}{
		{"owner only", 0o700, 0o700},
		{"group but not other", 0o750, 0o750},
		{"the ordinary install", 0o755, 0o755},
		{"no execute bit at all", 0o644, 0o744},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			exe := filepath.Join(dir, "synty-sync")
			if err := os.WriteFile(exe, fakeBinary("OLD"), 0o600); err != nil {
				t.Fatal(err)
			}
			// Set after the write, so the umask cannot decide what the test starts from.
			if err := os.Chmod(exe, tc.have); err != nil {
				t.Fatal(err)
			}
			srv, _ := assetServer(t, http.StatusOK, zipWith(t, installedBinaryName(), fakeBinary("NEW")))
			if err := installTo(context.Background(), "tok", srv.URL, exe); err != nil {
				t.Fatalf("installTo: %v", err)
			}
			fi, err := os.Stat(exe)
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode().Perm(); got != tc.want {
				t.Errorf("mode after update = %v, want %v from the %v install it replaced", got, tc.want, tc.have)
			}
		})
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

// The binary being replaced is the one running the update, so a download that fails has
// to leave it exactly as it was and take its staging directory with it. Moving the
// current binary aside before the new one is in hand would leave a user whose download
// 403'd with no synty-sync at all, and no way to run update again to fix it.
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

// The repo is private, so the update only works with a token, and install.sh resolves
// one in this same order. Reversing it hands GH_TOKEN (whatever `gh` happened to log in
// as last) precedence over the token the user set for this command, which on a machine
// with both silently updates from an account that may not have access at all.
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

// The branch that makes installTo's promise true — "a failure at any point leaves the
// working binary exactly as it was". Once exe has been renamed aside there is no binary
// at the install path, so a failed install has to put it back, and update is the one
// command that cannot be re-run to fix itself. Every other failure test stops before
// replaceBinary is reached (a failed download, a rejected asset) or forces the restore
// itself to fail, so a regression that restored from the wrong path, or returned early
// without restoring, deleted the user's only binary with the suite green.
func TestReplaceBinaryPutsTheWorkingBinaryBackWhenTheInstallFails(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "synty-sync")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A source that does not exist fails both the rename and the copy. The directory
	// stays writable, so the restore that follows can succeed.
	err := replaceBinary(filepath.Join(dir, "not-there"), exe)
	if err == nil {
		t.Fatal("replaceBinary reported success with nothing to install")
	}
	got, readErr := os.ReadFile(exe)
	if readErr != nil {
		t.Fatalf("the working binary is gone after a failed install: %v", readErr)
	}
	if string(got) != "old" {
		t.Errorf("the binary at the install path holds %q, want the original %q", got, "old")
	}
	info, statErr := os.Stat(exe)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("the restored binary is not executable (mode %v)", info.Mode())
	}
	if _, err := os.Stat(exe + ".old"); !os.IsNotExist(err) {
		t.Errorf("the aside copy was left behind at %s", exe+".old")
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

// Tags carry a leading v and main.version does not, except when it does, so the
// comparison has to be on the numbers rather than the strings. Getting it wrong makes
// every `update` on an already-current install download and swap the same binary over
// itself, which is the one operation in this package that cannot be undone if it goes
// wrong.
func TestRunStopsWhenAlreadyOnTheReleaseVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tag     string
		current string
		target  Requested
		want    string
		// wantPath is the release the request has to ask GitHub for. fetchRelease adds
		// the v itself, and nothing read the URL, so routing a targeted update at
		// /latest or dropping that prefix left the suite green while `update 1.2.3`
		// came back 404 — reported as "a token without access to this private repo
		// reads as 404", which sends the reader to audit a token over a URL.
		wantPath string
	}{
		{"latest, tag carries the v", "v1.2.3", "1.2.3", "", "already on the latest version (1.2.3)", "/latest"},
		{"explicit target", "v1.2.3", "1.2.3", "1.2.3", "already on the requested version (1.2.3)", "/tags/v1.2.3"},
		{"explicit target carrying its own v", "v1.2.3", "1.2.3", "v1.2.3", "already on the requested version (1.2.3)", "/tags/v1.2.3"},
		{"current carries the v too", "v1.2.3", "v1.2.3", "", "already on the latest version (1.2.3)", "/latest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
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
			if gotPath != tc.wantPath {
				t.Errorf("asked GitHub for %q, want %q", gotPath, tc.wantPath)
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
	rel := &release{Assets: []asset{
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

// The sniff is what stops a GitHub error page being chmod +x'd over a working install,
// and CI runs on one platform, so two of the three tables are never executed. Feeding
// each through the same function with goos as a parameter asserts all of them on one
// machine — including that each platform rejects the others' magic.
func TestExecutableMagicPerPlatform(t *testing.T) {
	heads := map[string][]byte{
		"linux":   []byte("\x7fELF\x02\x01"),
		"darwin":  {0xcf, 0xfa, 0xed, 0xfe},
		"windows": []byte("MZ\x90\x00"),
	}
	// The Mach-O variants the table accepts, all of which must pass on darwin.
	for _, h := range [][]byte{{0xce, 0xfa, 0xed, 0xfe}, {0xca, 0xfe, 0xba, 0xbe}} {
		if err := checkMagic("darwin", h); err != nil {
			t.Errorf("darwin rejected a Mach-O variant %x: %v", h, err)
		}
	}
	for goos, head := range heads {
		if err := checkMagic(goos, head); err != nil {
			t.Errorf("%s rejected its own magic: %v", goos, err)
		}
		for other := range heads {
			if other == goos {
				continue
			}
			if err := checkMagic(other, head); err == nil {
				t.Errorf("%s accepted %s's magic %x", other, goos, head)
			}
		}
		if err := checkMagic(goos, []byte("<!doctype html>")); err == nil {
			t.Errorf("%s accepted an HTML error page as a binary", goos)
		}
	}
	// A platform with no table is not second-guessed.
	if err := checkMagic("plan9", []byte("whatever")); err != nil {
		t.Errorf("an unknown platform was refused: %v", err)
	}
}

// checkMagic lets a GOOS with no table through, so an unlisted platform stays updatable.
// That fail-open is only safe while every platform the release actually builds has a
// signature: adding one to release.yml and to assetSuffix without one here turns the last
// guard before the rename into a no-op on that platform alone, and an error page shipped
// as the asset is renamed over the working binary and reported as an update.
func TestEveryReleasedPlatformHasAnExecutableSignature(t *testing.T) {
	for _, p := range releasePlatforms(t) {
		if len(executableMagic[p.GOOS]) == 0 {
			t.Errorf("release.yml builds %s/%s but executableMagic has no signature for %s, "+
				"so checkExecutable would accept anything there", p.GOOS, p.GOARCH, p.GOOS)
			continue
		}
		if err := checkMagic(p.GOOS, []byte("<!doctype html>")); err == nil {
			t.Errorf("%s accepted an HTML error page as a binary", p.GOOS)
		}
	}
}

// An asset request to api.github.com is answered with a 302 to a CDN host whose query
// carries a signature: a live bearer credential for a private release asset.
// net/http reports a failure on a redirected request against the last URL it tried,
// stripping only the userinfo password, so the signature rides into the error text
// and out to stderr unless the query is dropped.
func TestDownloadErrorAfterARedirectDropsTheSignature(t *testing.T) {
	// A port nothing is listening on, so the hop after the redirect fails at dial.
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadURL := "http://" + dead.Addr().String()
	if err := dead.Close(); err != nil {
		t.Fatal(err)
	}

	const secret = "SIGNATURE-THAT-MUST-NOT-BE-PRINTED"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, deadURL+"/release-assets/synty-sync.zip?X-Amz-Signature="+secret, http.StatusFound)
	}))
	defer srv.Close()

	err = download(context.Background(), "tok", srv.URL+"/asset", filepath.Join(t.TempDir(), "out.zip"))
	if err == nil {
		t.Fatal("expected the download to fail at the redirect target")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the signed asset URL leaked into the error: %q", err)
	}
	// Still useful: the host is what tells a user which hop failed.
	if !strings.Contains(err.Error(), "downloading") {
		t.Errorf("error lost its context: %q", err)
	}
}

// GitHub answers 404 rather than 403 for a private repo the caller cannot see, so a
// token that simply lacks access to this repo produces "no releases found", which is
// false, and leaves the user with nothing to check. Both branches have to point
// somewhere.
func TestNotFoundAlwaysSaysSomethingAboutTheToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	t.Cleanup(func(prev string) func() { return func() { releasesAPIURL = prev } }(releasesAPIURL))
	releasesAPIURL = srv.URL

	for _, tc := range []struct{ name, token, want string }{
		{"no token at all", "", "gh auth login"},
		{"a token that cannot see the repo", "ghp_SECRET_VALUE_9876", "gh auth status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fetchRelease(context.Background(), tc.token, "")
			if err == nil {
				t.Fatal("expected a not-found error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not point at %q", err, tc.want)
			}
			if strings.Contains(err.Error(), tc.token) && tc.token != "" {
				t.Errorf("the token leaked into the error: %q", err)
			}
		})
	}
}

// This is the one request in the update path that carries an Authorization header, and
// the only error site that relayed a response body verbatim and unbounded. GitHub
// answers an incident with a full HTML page, and so do captive portals and intercepting
// proxies — which is where relaying the body stops being merely noisy, since some of
// them echo the request back. Only GitHub's own JSON travels now; anything else is
// reported as the status it arrived with.
func TestFetchReleaseDoesNotRelayANonJSONErrorBody(t *testing.T) {
	const page = "<html><head><title>502 Bad Gateway</title></head><body>" +
		"proxy-secret-echo Authorization: Bearer hunter2</body></html>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, page+strings.Repeat(" padding", 5000))
	}))
	defer srv.Close()

	apiWas := releasesAPIURL
	releasesAPIURL = srv.URL + "/"
	defer func() { releasesAPIURL = apiWas }()

	_, err := fetchRelease(context.Background(), "", "")
	if err == nil {
		t.Fatal("a 502 produced no error")
	}
	msg := err.Error()
	if strings.Contains(msg, "hunter2") || strings.Contains(msg, "<html>") {
		t.Errorf("the error relayed an intermediary's page:\n%s", msg)
	}
	if len(msg) > 200 {
		t.Errorf("error is %d bytes; an unbounded body reached it:\n%s", len(msg), msg)
	}
	if !strings.Contains(msg, "502") {
		t.Errorf("error does not name the status: %s", msg)
	}
}

// The real GitHub message is what makes a 422 or a rate-limit readable, so bounding the
// body must not throw it away.
func TestFetchReleaseKeepsGitHubsOwnJSONMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"API rate limit exceeded","documentation_url":"https://docs.github.com"}`)
	}))
	defer srv.Close()

	apiWas := releasesAPIURL
	releasesAPIURL = srv.URL + "/"
	defer func() { releasesAPIURL = apiWas }()

	_, err := fetchRelease(context.Background(), "", "")
	if err == nil {
		t.Fatal("a 403 produced no error")
	}
	if !strings.Contains(err.Error(), "API rate limit exceeded") {
		t.Errorf("GitHub's own message was dropped: %s", err)
	}
}

// Every platform's suffix has to match a label in release.yml, and the workflow is
// the only place those labels are really decided. Asserting platformAsset against a
// list this test writes down would just be a fourth copy of them: renaming a label
// in the workflow would leave every check green and ship "no asset for your
// platform" to the one platform CI does not run on. So read the workflow.
//
// Both directions are checked against assetSuffix rather than against a synthesized
// release. Asking platformAsset instead cannot see a label the workflow *stopped*
// publishing: it only ever returns a URL for an asset that is present, so a reverse
// check built on it passes by construction, and dropping a platform from the array
// silently drops it from this test too.
func TestPlatformAssetMatchesTheLabelsReleaseBuilds(t *testing.T) {
	built := releasePlatforms(t)

	rel := &release{}
	for _, p := range built {
		rel.Assets = append(rel.Assets, asset{Name: "synty-sync-1.0.0-" + p.Label + ".zip", URL: "u/" + p.Label})
	}

	// Forward: every platform the workflow builds resolves to that platform's asset.
	for _, p := range built {
		t.Run(p.GOOS+"/"+p.GOARCH, func(t *testing.T) {
			label, err := assetSuffix(p.GOOS, p.GOARCH)
			if err != nil {
				t.Fatalf("release.yml builds %s/%s but the updater has no label for it: %v", p.GOOS, p.GOARCH, err)
			}
			if label != p.Label {
				t.Errorf("the updater wants %q, but release.yml publishes %q", label, p.Label)
			}
			url, err := platformAsset(rel, p.GOOS, p.GOARCH)
			if err != nil {
				t.Fatalf("release.yml builds %s/%s but the updater cannot find it: %v", p.GOOS, p.GOARCH, err)
			}
			if url != "u/"+p.Label {
				t.Errorf("platformAsset = %q, want the %q asset release.yml publishes", url, p.Label)
			}
		})
	}

	// Reverse: every platform the updater claims is one the workflow builds. This is
	// what catches a platform removed from the array, which the forward loop cannot see
	// because it iterates that same array.
	//
	// Keyed on the (GOOS, GOARCH) pair, not on the label. A set of labels answers
	// "is this string published", which windows/arm64 satisfies through the
	// windows/amd64 entry, so the updater could claim a platform no release builds and
	// this loop would agree. The arch list runs past the two the workflow builds for
	// the same reason: a resolver that reads only GOOS never gets asked about the
	// architectures it is silently folding together.
	builds := map[string]string{}
	for _, p := range built {
		builds[p.GOOS+"/"+p.GOARCH] = p.Label
	}
	for _, goos := range []string{"darwin", "linux", "windows", "freebsd"} {
		for _, goarch := range []string{"amd64", "arm64", "386", "arm", "riscv64", "ppc64le"} {
			label, err := assetSuffix(goos, goarch)
			if err != nil {
				continue // the updater does not claim this platform
			}
			want, isBuilt := builds[goos+"/"+goarch]
			if !isBuilt {
				t.Errorf("the updater resolves %s/%s to %q, but release.yml does not build that pair; "+
					"`update` there swaps another architecture's binary over a working one and removes it",
					goos, goarch, label)
				continue
			}
			if label != want {
				t.Errorf("the updater resolves %s/%s to %q, but release.yml publishes it as %q", goos, goarch, label, want)
			}
		}
	}

	if _, err := assetSuffix("plan9", "amd64"); err == nil {
		t.Error("an unsupported platform resolved to a label")
	}
}

// releasePlatforms reads the platforms the release workflow actually builds, so the
// asset labels have exactly one source of truth.
func releasePlatforms(t *testing.T) []releaseyml.Platform {
	t.Helper()
	out, err := releaseyml.Platforms(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	return out
}
