package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// installerZip builds a release archive holding one file named synty-sync with the
// given bytes, so a test can ship either a real-looking binary or something that is
// not a binary at all.
func installerZip(t *testing.T, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("synty-sync")
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

// nativeMagic is the leading signature the installer checks for on this platform.
func nativeMagic(t *testing.T) []byte {
	t.Helper()
	switch runtime.GOOS {
	case "linux":
		return []byte("\x7fELF")
	case "darwin":
		return []byte{0xcf, 0xfa, 0xed, 0xfe}
	}
	t.Skipf("install.sh supports linux and darwin, not %s", runtime.GOOS)
	return nil
}

// stubRelease serves the two GitHub endpoints the installer reads plus the asset
// itself, so the script can be run end to end without network.
func stubRelease(t *testing.T, asset []byte) *httptest.Server {
	t.Helper()
	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/curbol/synty-sync/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name": "v9.9.9"}`)
	})
	mux.HandleFunc("/repos/curbol/synty-sync/releases/tags/v9.9.9", func(w http.ResponseWriter, r *http.Request) {
		// The shape the installer greps: a "url" line within three lines of "name".
		fmt.Fprintf(w, "{\n  \"assets\": [\n    {\n      \"url\": \"%s/asset\",\n      \"x\": 1,\n      \"y\": 2,\n      \"name\": \"%s\"\n    }\n  ]\n}\n",
			base, "synty-sync-9.9.9-"+platformLabel(t)+".zip")
	})
	mux.HandleFunc("/asset", func(w http.ResponseWriter, r *http.Request) {
		w.Write(asset)
	})
	// The unauthenticated route, so no test can reach the real github.com.
	mux.HandleFunc("/curbol/synty-sync/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		w.Write(asset)
	})
	srv := httptest.NewServer(mux)
	base = srv.URL
	t.Cleanup(srv.Close)
	return srv
}

func platformLabel(t *testing.T) string {
	t.Helper()
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		return "linux-intel"
	case "linux/arm64":
		return "linux-arm64"
	case "darwin/amd64":
		return "mac-intel"
	case "darwin/arm64":
		return "mac-apple"
	}
	t.Skipf("no release label for %s/%s", runtime.GOOS, runtime.GOARCH)
	return ""
}

// runInstaller runs install.sh with a throwaway HOME and no token in the
// environment, returning its combined output and exit error.
func runInstaller(t *testing.T, home string, env ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("unzip"); err != nil {
		t.Skip("install.sh needs unzip")
	}
	cmd := exec.Command("bash", "install.sh")
	cmd.Env = append(os.Environ(), "HOME="+home)
	// The installer falls back to the gh CLI, which on a developer machine is logged
	// in; clear the whole token path so the test controls it.
	cmd.Env = append(cmd.Env, "GITHUB_TOKEN=", "GH_TOKEN=", "PATH=/usr/bin:/bin")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// The ordinary no-token case must say so. auth_header returns non-zero when it finds
// nothing, and under `set -e` a bare assignment from it killed the script before any
// of the messages written for this case could print.
func TestInstallerReportsAnUnreachableRelease(t *testing.T) {
	home := t.TempDir()
	out, err := runInstaller(t, home,
		"SYNTY_INSTALL_API=http://127.0.0.1:1", "SYNTY_INSTALL_DOWNLOAD=http://127.0.0.1:1")
	if err == nil {
		t.Fatalf("the installer succeeded against an unreachable release:\n%s", out)
	}
	if !strings.Contains(out, "could not resolve latest version") {
		t.Errorf("the installer failed with no explanation:\n%s", out)
	}
}

// A release that shipped something that is not a binary must not be chmod +x'd over
// a working install, and must not report success.
func TestInstallerRefusesANonExecutableAsset(t *testing.T) {
	nativeMagic(t)
	home := t.TempDir()
	installed := filepath.Join(home, ".local", "bin", "synty-sync")
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, append(nativeMagic(t), []byte("the working one")...), 0o755); err != nil {
		t.Fatal(err)
	}

	srv := stubRelease(t, installerZip(t, []byte("<!doctype html><title>Not found</title>")))
	out, err := runInstaller(t, home, "GITHUB_TOKEN=test-token",
		"SYNTY_INSTALL_API="+srv.URL, "SYNTY_INSTALL_DOWNLOAD="+srv.URL)
	if err == nil {
		t.Fatalf("a non-executable asset installed successfully:\n%s", out)
	}
	if !strings.Contains(out, "not a") || !strings.Contains(out, "executable") {
		t.Errorf("the asset was rejected for some other reason than not being a binary:\n%s", out)
	}
	got, readErr := os.ReadFile(installed)
	if readErr != nil {
		t.Fatalf("the working binary is gone: %v", readErr)
	}
	if !strings.Contains(string(got), "the working one") {
		t.Errorf("the working binary was replaced with a document:\n%s", out)
	}
}

// The staging directory must not survive, on success or on failure: it sits inside
// the install directory so the final move is an atomic same-filesystem rename.
func TestInstallerInstallsAndLeavesNoStagingBehind(t *testing.T) {
	want := append(nativeMagic(t), []byte("a real enough binary")...)
	home := t.TempDir()
	srv := stubRelease(t, installerZip(t, want))

	// The smoke test at the end runs the installed file, which is not a real binary
	// here, so a non-zero exit is expected — the install itself must still be right.
	out, _ := runInstaller(t, home, "GITHUB_TOKEN=test-token",
		"SYNTY_INSTALL_API="+srv.URL, "SYNTY_INSTALL_DOWNLOAD="+srv.URL)

	binDir := filepath.Join(home, ".local", "bin")
	got, err := os.ReadFile(filepath.Join(binDir, "synty-sync"))
	if err != nil {
		t.Fatalf("nothing was installed: %v\n%s", err, out)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("installed %d bytes, want the %d from the asset", len(got), len(want))
	}
	info, err := os.Stat(filepath.Join(binDir, "synty-sync"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("the installed binary is not executable (mode %v)", info.Mode())
	}
	entries, err := os.ReadDir(binDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".synty-sync-install-") {
			t.Errorf("a staging directory was left behind: %s", e.Name())
		}
	}
}
