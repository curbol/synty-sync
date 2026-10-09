package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/curbol/synty-sync/internal/releaseyml"
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
	return stubReleaseShaped(t, asset, assetShape{})
}

// assetShape is how an asset object in the release payload is arranged. The installer
// reads the asset's API URL out of that payload by text, so neither the distance
// between "url" and "name" nor their order may decide which asset it resolves.
type assetShape struct {
	// extraFields is how many keys sit between "url" and "name", standing in for GitHub
	// adding one (it has already added "digest" once).
	extraFields int
	// nameFirst puts "name" ahead of "url" in the object, which is the arrangement that
	// makes a search for the nearest id above the name resolve the asset before it.
	nameFirst bool
	// compact puts the whole payload on one line. GitHub pretty-prints today and that
	// is not a contract; both of the installer's text reads have to hold either way.
	compact bool
}

// spaced renders a release payload the way the stub is configured to: as GitHub
// pretty-prints it today, or on the one line its compact form uses. Errorf rather than
// Fatalf because this runs on a server goroutine, where a Goexit would abort the
// response mid-write instead of failing the test.
func spaced(t *testing.T, payload string, compact bool) string {
	t.Helper()
	if !compact {
		return payload
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(payload)); err != nil {
		t.Errorf("the release fixture is not valid JSON: %v", err)
		return payload
	}
	return buf.String()
}

// stubReleaseShaped is stubRelease serving an asset object arranged as shape says.
func stubReleaseShaped(t *testing.T, asset []byte, shape assetShape) *httptest.Server {
	return stubReleaseWatching(t, asset, shape, nil)
}

// stubReleaseWatching is stubReleaseShaped with a hook that runs while the asset
// request is in flight, for a caller that has to observe the installer's own working
// state rather than what it leaves behind.
func stubReleaseWatching(t *testing.T, asset []byte, shape assetShape, onAsset func()) *httptest.Server {
	t.Helper()
	// Resolved on the test goroutine, before any handler can run: platformLabel can
	// call t.Skipf, and a Goexit from a server goroutine would abort a response
	// mid-write rather than skip the test.
	return stubReleaseLabeled(t, asset, platformLabel(t), shape, onAsset)
}

// stubReleaseLabeled is stubReleaseWatching for a caller that drives install.sh with a
// stubbed uname, where the platform the installer resolves is not the host's.
func stubReleaseLabeled(t *testing.T, asset []byte, label string, shape assetShape, onAsset func()) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	// The repo is private, so every one of these routes is a 404 without the token,
	// exactly as github.com behaves. A stub that answered anyway would let the
	// installer lose its auth entirely (an empty AUTH_CONF, a mktemp change, a call to
	// ensure_auth_config from a subshell) and still pass every test here, while every
	// real user got "could not resolve the latest release".
	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "token "+stubToken {
				t.Errorf("%s reached the stub without the auth header; the installer lost its token", r.URL.Path)
				http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/repos/curbol/synty-sync/releases/latest", authed(func(w http.ResponseWriter, r *http.Request) {
		// The whole release object, the same one the tags route serves. A lone
		// {"tag_name": ...} is a payload where the tag is also the last quoted run in
		// the document, so a greedy read of it and a correct one agree — and the
		// installer reads this by text, so the fixture has to be able to disagree.
		fmt.Fprint(w, spaced(t, githubReleaseJSON(r.Host, "synty-sync-9.9.9-"+label+".zip", shape), shape.compact))
	}))
	mux.HandleFunc("/repos/curbol/synty-sync/releases/tags/v9.9.9", authed(func(w http.ResponseWriter, r *http.Request) {
		// GitHub's real asset object, keys in the order the API returns them and the
		// uploader block included. A hand-made object with "url" three lines above
		// "name" would assert the installer's own assumption about the JSON's shape
		// rather than test the parse: the fixture and the grep would agree with each
		// other and disagree with GitHub the moment a field is added ahead of "name".
		// The asset URL is built from the request's own Host rather than a variable the
		// test goroutine writes after the server is already serving.
		fmt.Fprint(w, spaced(t, githubReleaseJSON(r.Host, "synty-sync-9.9.9-"+label+".zip", shape), shape.compact))
	}))
	mux.HandleFunc("/repos/curbol/synty-sync/releases/assets/", authed(func(w http.ResponseWriter, r *http.Request) {
		// The wanted asset is id 2; resolving to the decoy's id means the parse picked
		// the wrong object out of the list.
		if !strings.HasSuffix(r.URL.Path, "/2") {
			t.Errorf("the installer resolved %s, not the asset matching its platform label", r.URL.Path)
			http.Error(w, "wrong asset", http.StatusNotFound)
			return
		}
		if onAsset != nil {
			onAsset()
		}
		w.Write(asset)
	}))
	// Registered so no test can reach the real github.com, but reaching it at all is
	// the failure: it is the path a private repo cannot serve, and it used to hand back
	// the same bytes as the authenticated one, which is what hid the whole question.
	mux.HandleFunc("/curbol/synty-sync/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the installer fell back to the unauthenticated download path (%s); a private repo answers that with a 404", r.URL.Path)
		http.Error(w, "not found", http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// githubReleaseJSON renders a release payload shaped like the one api.github.com
// returns: every key an asset object carries, in the API's order, with the uploader
// block and a second asset ahead of the wanted one. install.sh reads the asset URL out
// of this by text, so what it is read out of has to look like the real thing rather
// than like whatever the reader currently happens to need.
func githubReleaseJSON(host, wantName string, shape assetShape) string {
	var padding strings.Builder
	for i := range shape.extraFields {
		fmt.Fprintf(&padding, "\n      \"field_github_added_%d\": null,", i)
	}
	// Argument indexes are explicit throughout, because nameFirst reorders the verbs
	// and a sequential %s after a reordered head would silently pick up the wrong one.
	head := `    {
      "url": "http://%[1]s/repos/curbol/synty-sync/releases/assets/%[2]d",
      "id": %[3]d,
      "node_id": "RA_kwDOAbCdEf4AAAAA",` + padding.String() + `
      "name": %[4]q,
      "label": null,`
	if shape.nameFirst {
		head = `    {
      "name": %[4]q,
      "id": %[3]d,
      "node_id": "RA_kwDOAbCdEf4AAAAA",` + padding.String() + `
      "url": "http://%[1]s/repos/curbol/synty-sync/releases/assets/%[2]d",
      "label": null,`
	}
	asset := func(id int, name string) string {
		return fmt.Sprintf(head+`
      "uploader": {
        "login": "curbol",
        "id": 1,
        "node_id": "MDQ6VXNlcjE=",
        "avatar_url": "https://avatars.githubusercontent.com/u/1?v=4",
        "url": "http://%[5]s/users/curbol",
        "html_url": "https://github.com/curbol",
        "type": "User",
        "site_admin": false
      },
      "content_type": "application/zip",
      "state": "uploaded",
      "size": 4096,
      "digest": "sha256:0000000000000000000000000000000000000000000000000000000000000000",
      "download_count": 0,
      "created_at": "2026-01-01T00:00:00Z",
      "updated_at": "2026-01-01T00:00:00Z",
      "browser_download_url": "http://%[6]s/curbol/synty-sync/releases/download/v9.9.9/%[7]s"
    }`, host, id, id, name, host, host, name)
	}
	return fmt.Sprintf(`{
  "tag_name": "v9.9.9",
  "assets": [
%s,
%s
  ]
}
`, asset(1, "synty-sync-9.9.9-some-other-platform.zip"), asset(2, wantName))
}

// stubToken is the token every installer test passes, and the one stubRelease
// requires. It is not a credential shape on purpose: nothing here should resemble a
// real GitHub token.
const stubToken = "test-token"

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
// environment, returning its combined output and exit error. Any token the caller
// does supply is checked against the output before it is returned: the installer is
// the half a user pipes into a shell, and its Go twin already guards this
// (TestFetchReleaseErrorOmitsToken), so the check belongs where every test that
// passes a token gets it rather than on the two that happen to remember.
// ghStub returns a directory holding a gh that prints token and succeeds, or reports no
// token when token is empty. gh lives in /usr/bin on an ordinary developer machine, so
// clearing GITHUB_TOKEN and GH_TOKEN is not enough to control install.sh's token path:
// without shadowing it the no-token tests find a logged-in CLI and stop testing
// anything, and the gh tier cannot be exercised on a machine where gh is absent.
func ghStub(t *testing.T, token string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nexit 1\n"
	if token != "" {
		script = "#!/bin/sh\n[ \"$1 $2\" = \"auth token\" ] || exit 1\nprintf %s " + token + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runInstaller(t *testing.T, home string, env ...string) (string, error) {
	t.Helper()
	return runInstallerWithGh(t, home, "", env...)
}

// runInstallerWithGh is runInstaller with a gh on PATH that hands back ghToken, so the
// tier install.sh reaches only when neither environment variable is set can be driven
// on a machine where gh is absent or logged out.
func runInstallerWithGh(t *testing.T, home, ghToken string, env ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("unzip"); err != nil {
		t.Skip("install.sh needs unzip")
	}
	cmd := exec.Command("bash", "install.sh")
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Env = append(cmd.Env, "GITHUB_TOKEN=", "GH_TOKEN=", "PATH="+ghStub(t, ghToken)+":/usr/bin:/bin")
	cmd.Env = append(cmd.Env, env...)
	raw, err := cmd.CombinedOutput()
	out := string(raw)
	for _, e := range env {
		for _, key := range []string{"GITHUB_TOKEN=", "GH_TOKEN="} {
			token, ok := strings.CutPrefix(e, key)
			if !ok || token == "" {
				continue
			}
			if strings.Contains(out, token) {
				t.Errorf("the installer put %s in its output:\n%s", strings.TrimSuffix(key, "="), out)
			}
		}
	}
	// A token from the gh CLI is as much a credential as one from the environment.
	if ghToken != "" && strings.Contains(out, ghToken) {
		t.Errorf("the installer put the gh CLI's token in its output:\n%s", out)
	}
	return out, err
}

// The ordinary no-token case must say so. auth_header returns non-zero when it finds
// nothing, and under `set -e` a bare assignment from it killed the script before any
// of the messages written for this case could print.
//
// A token that cannot see the repo is the other half. GitHub answers 404 rather than
// 403 for a private repo the caller cannot read, so both arrive here as an empty
// result: telling someone who has already exported a token to export one sends them to
// check the thing that is not wrong.
func TestInstallerReportsAnUnreachableRelease(t *testing.T) {
	for _, tc := range []struct{ name, ghToken, want, notWant string }{
		{"no token at all", "", "no GitHub token found", "does not have access"},
		{"a token without access", "wrong-org-token", "does not have access", "no GitHub token found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			out, err := runInstallerWithGh(t, home, tc.ghToken,
				"SYNTY_INSTALL_API=http://127.0.0.1:1", "SYNTY_INSTALL_DOWNLOAD=http://127.0.0.1:1")
			if err == nil {
				t.Fatalf("the installer succeeded against an unreachable release:\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("the installer did not say %q:\n%s", tc.want, out)
			}
			if strings.Contains(out, tc.notWant) {
				t.Errorf("the installer gave the other case's advice (%q):\n%s", tc.notWant, out)
			}
		})
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
	assertNoStagingLeft(t, filepath.Dir(installed))
	got, readErr := os.ReadFile(installed)
	if readErr != nil {
		t.Fatalf("the working binary is gone: %v", readErr)
	}
	if !strings.Contains(string(got), "the working one") {
		t.Errorf("the working binary was replaced with a document:\n%s", out)
	}
}

// install.sh resolves a token from GITHUB_TOKEN, then GH_TOKEN, then `gh auth token`,
// the same order selfupdate.resolveToken uses. Only the first tier was ever exercised:
// every test that supplied a token supplied GITHUB_TOKEN, and runInstaller shadows gh
// with a stub that reports none. Simplifying auth_token to ${GITHUB_TOKEN:-$GH_TOKEN},
// or reordering the `command -v gh` guard, left the suite green while the user whose only
// credential is GH_TOKEN or a logged-in gh got "private repo needs gh auth or
// GITHUB_TOKEN", which reads as their setup being wrong.
func TestInstallerAcceptsEveryCredentialSource(t *testing.T) {
	want := append(nativeMagic(t), []byte("a real enough binary")...)
	for _, tc := range []struct {
		name    string
		env     []string
		ghToken string
	}{
		{name: "GITHUB_TOKEN", env: []string{"GITHUB_TOKEN=" + stubToken}},
		{name: "GH_TOKEN", env: []string{"GH_TOKEN=" + stubToken}},
		{name: "gh auth token", ghToken: stubToken},
		// Presence is not order. With one credential available per case, swapping the
		// operands of ${GITHUB_TOKEN:-${GH_TOKEN:-}} or consulting gh first leaves every
		// row above green, while a machine where gh is logged in to a personal account
		// and GITHUB_TOKEN is the one scoped to this repo silently picks the wrong
		// credential and reports the asset as missing from the release.
		{
			name:    "GITHUB_TOKEN wins over GH_TOKEN and the gh CLI",
			env:     []string{"GITHUB_TOKEN=" + stubToken, "GH_TOKEN=wrong-gh-token"},
			ghToken: "wrong-cli-token",
		},
		{
			name:    "GH_TOKEN wins over the gh CLI",
			env:     []string{"GH_TOKEN=" + stubToken},
			ghToken: "wrong-cli-token",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			srv := stubRelease(t, installerZip(t, want))
			env := append(tc.env, "SYNTY_INSTALL_API="+srv.URL, "SYNTY_INSTALL_DOWNLOAD="+srv.URL)
			// The smoke test at the end runs the installed file, which is not a real
			// binary here, so a non-zero exit is expected. stubRelease fails any request
			// that arrives without the header, so reaching the asset at all is the proof
			// the token was found.
			out, _ := runInstallerWithGh(t, home, tc.ghToken, env...)
			got, err := os.ReadFile(filepath.Join(home, ".local", "bin", "synty-sync"))
			if err != nil {
				t.Fatalf("nothing installed from a %s credential: %v\n%s", tc.name, err, out)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("installed bytes differ from the asset:\n%s", out)
			}
		})
	}
}

// install.sh finds the asset's API URL by text, because the private-repo download needs
// the id and there is no jq on a fresh machine. What must not matter is how that object
// is arranged. Distance first: GitHub puts "url" and "name" three lines apart today with
// no margin and has already added a key to this object once ("digest"), and one field
// ahead of "name" used to drop the URL out of the grep's window, leaving every fresh
// install with "asset not found in release" while everyone who already had a binary kept
// updating fine. Order second, and it is the worse failure: searching above the name for
// the nearest id resolves the *previous* asset once "name" comes first, and between
// linux-intel and linux-arm64 both are ELF, so the magic check passes, the binary is
// replaced, and the smoke test is the first thing that notices.
func TestInstallerFindsTheAssetHoweverGitHubShapesTheObject(t *testing.T) {
	want := append(nativeMagic(t), []byte("a real enough binary")...)
	for _, shape := range []assetShape{
		{extraFields: 1},
		{extraFields: 8},
		{nameFirst: true},
		{nameFirst: true, extraFields: 8},
	} {
		t.Run(fmt.Sprintf("%+v", shape), func(t *testing.T) {
			home := t.TempDir()
			srv := stubReleaseShaped(t, installerZip(t, want), shape)
			// The smoke test at the end runs the installed file, which is not a real
			// binary here, so a non-zero exit is expected; what matters is that the right
			// asset was resolved and landed.
			out, _ := runInstaller(t, home, "GITHUB_TOKEN="+stubToken,
				"SYNTY_INSTALL_API="+srv.URL, "SYNTY_INSTALL_DOWNLOAD="+srv.URL)
			if strings.Contains(out, "not found in release") {
				t.Fatalf("the asset URL was not found in an object shaped %+v:\n%s", shape, out)
			}
			got, err := os.ReadFile(filepath.Join(home, ".local", "bin", "synty-sync"))
			if err != nil {
				t.Fatalf("nothing installed: %v\n%s", err, out)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("installed bytes differ from the asset:\n%s", out)
			}
		})
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
	assertNoStagingLeft(t, binDir)
}

// The binary is moved into place with mv, which is atomic only within one
// filesystem, so staging has to sit inside the install directory. Staging under the
// system temp dir still installs — mv degrades to copy-then-unlink across a boundary
// — so every test here passes either way while an interruption starts being able to
// leave a truncated binary at the live path. Checked while the asset is in flight,
// because the trap removes the directory before the installer exits.
func TestInstallerStagesInsideTheInstallDirectory(t *testing.T) {
	home := t.TempDir()
	binDir := filepath.Join(home, ".local", "bin")
	// Written on the server goroutine and read on the test's, so it is atomic.
	var staged atomic.Bool
	srv := stubReleaseWatching(t, installerZip(t, append(nativeMagic(t), []byte("binary")...)), assetShape{}, func() {
		entries, err := os.ReadDir(binDir)
		if err != nil {
			return // the installer has not created the install dir yet
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".synty-sync-install-") {
				staged.Store(true)
			}
		}
	})

	out, _ := runInstaller(t, home, "GITHUB_TOKEN=test-token",
		"SYNTY_INSTALL_API="+srv.URL, "SYNTY_INSTALL_DOWNLOAD="+srv.URL)

	if !staged.Load() {
		t.Errorf("the installer did not stage inside %s, so the final mv can cross a filesystem:\n%s", binDir, out)
	}
}

// check_executable switches on the live `uname -s`, so on a Linux runner only the ELF
// arm ever runs and the macOS table is carried untested: a typo or a reorder in it
// ships green and either rejects every valid macOS install or chmod +x's an error page
// over one. The Go twin takes goos as a parameter precisely so all of its tables are
// asserted on one machine; this drives the same coverage through a stubbed uname.
func TestInstallerChecksMacMagicOnADarwinHost(t *testing.T) {
	for _, tc := range []struct {
		name   string
		magic  []byte
		wantOK bool
	}{
		{"64-bit Mach-O", []byte{0xcf, 0xfa, 0xed, 0xfe}, true},
		{"32-bit Mach-O", []byte{0xce, 0xfa, 0xed, 0xfe}, true},
		{"universal binary", []byte{0xca, 0xfe, 0xba, 0xbe}, true},
		{"an error page served as the asset", []byte("<!doctype html><title>x</title>"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			srv := stubReleaseLabeled(t, installerZip(t, append(tc.magic, []byte(" body")...)), "mac-intel", assetShape{}, nil)

			// Darwin/x86_64 resolves to mac-intel, which is the label the stub serves.
			out, _ := runInstaller(t, home, "GITHUB_TOKEN=test-token",
				"SYNTY_INSTALL_API="+srv.URL, "SYNTY_INSTALL_DOWNLOAD="+srv.URL,
				"PATH="+unameStub(t, "Darwin", "x86_64")+":"+ghStub(t, "")+":/usr/bin:/bin")

			// The installed file, not the exit status: the smoke test at the end runs
			// the binary, and a Mach-O does not run on the machine this test does.
			_, err := os.Stat(filepath.Join(home, ".local", "bin", "synty-sync"))
			if tc.wantOK && err != nil {
				t.Errorf("a valid macOS binary was refused:\n%s", out)
			}
			if !tc.wantOK {
				if err == nil {
					t.Errorf("a non-executable asset was installed over the binary:\n%s", out)
				}
				if !strings.Contains(out, "not a macOS executable") {
					t.Errorf("refusal did not name the macOS check:\n%s", out)
				}
			}
		})
	}
}

// assertNoStagingLeft checks the install directory holds the binary and nothing else.
// The trap that removes the staging directory fires on every exit, so this belongs on
// the failure paths as much as the success one — those are the ones that depend on the
// trap at all.
//
// The exact set, not a scan for the staging prefix: the prefix comes from a mktemp
// template in install.sh, so a scan passes vacuously the moment that template changes.
// Both callers require a binary at this path anyway, one freshly installed and one that
// had to survive a refusal, so a missing directory is a failure rather than a pass.
func assertNoStagingLeft(t *testing.T, binDir string) {
	t.Helper()
	entries, err := os.ReadDir(binDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "synty-sync" {
		t.Errorf("the install dir holds %v, want only synty-sync; staging was left behind", names)
	}
}

// releasePlatforms reads what the release workflow actually builds. The installer
// composes the same labels from uname output in its own language, and the workflow
// is the only place they are really decided.
func releasePlatforms(t *testing.T) []releaseyml.Platform {
	t.Helper()
	out, err := releaseyml.Platforms(filepath.Join(".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// unameStub puts a uname on PATH that answers for the given platform, so the
// installer's real detect_platform can be run for a machine this one is not.
func unameStub(t *testing.T, sysname, machine string) string {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\n  -s) echo %q ;;\n  -m) echo %q ;;\nesac\n", sysname, machine)
	if err := os.WriteFile(filepath.Join(dir, "uname"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The installer builds the asset label from uname in shell, the updater builds it in
// Go, and release.yml decides what is actually published. Nothing compiles the three
// together, so a label renamed in the workflow leaves every check green and breaks
// installs on the one platform CI does not run. Run the real detect_platform for
// each pair the workflow builds and hold it to the published label.
func TestInstallerPlatformLabelsMatchTheRelease(t *testing.T) {
	unameFor := map[string][]string{ // goos/goarch -> the uname -s, -m spellings to try
		"darwin/amd64": {"Darwin", "x86_64"}, "darwin/arm64": {"Darwin", "arm64"},
		"linux/amd64": {"Linux", "x86_64"}, "linux/arm64": {"Linux", "aarch64"},
	}
	// The other spelling of each arch, so both arms of the installer's case are held
	// to the same label rather than only the one this machine reports.
	alias := map[string]string{"x86_64": "amd64", "aarch64": "arm64", "arm64": "aarch64"}

	covered := 0
	for _, p := range releasePlatforms(t) {
		if p.GOOS == "windows" {
			continue // install.sh refuses Windows by design; the release zip is used directly
		}
		un, ok := unameFor[p.GOOS+"/"+p.GOARCH]
		if !ok {
			t.Errorf("release.yml builds %s/%s but this guard does not know its uname output", p.GOOS, p.GOARCH)
			continue
		}
		covered++
		for _, machine := range []string{un[1], alias[un[1]]} {
			t.Run(p.Label+"/"+machine, func(t *testing.T) {
				out := runInstallerAs(t, unameStub(t, un[0], machine))
				// The whole line, terminator included: a label that merely starts with
				// the expected one ("mac-applesilicon" for "mac-apple") names an asset
				// no release publishes and must not read as a match.
				want := "INFO: platform: " + p.Label + "\n"
				if !strings.Contains(out, want) {
					t.Errorf("install.sh reported no %q for uname -s %q -m %q; release.yml publishes %s.zip\n%s",
						want, un[0], machine, p.Label, out)
				}
			})
		}
	}
	if covered == 0 {
		t.Fatal("no installable platform found in release.yml")
	}
	// The label this package's own test harness serves has to be the same one, or the
	// end-to-end installer tests would pass against an asset no release publishes.
	for _, p := range releasePlatforms(t) {
		if p.GOOS == runtime.GOOS && p.GOARCH == runtime.GOARCH && platformLabel(t) != p.Label {
			t.Errorf("platformLabel = %q, want the %q release.yml publishes for this host", platformLabel(t), p.Label)
		}
	}
}

// runInstallerAs runs install.sh with pathPrefix ahead of the system directories and
// an unreachable release, so it gets as far as announcing the platform and no
// further. Its non-zero exit is the expected outcome, not a failure.
func runInstallerAs(t *testing.T, pathPrefix string) string {
	t.Helper()
	// The same gh shadow runInstaller installs, and for the same reason: gh lives in
	// /usr/bin on a developer machine, so clearing the two environment variables still
	// leaves install.sh a logged-in CLI to find. Without this the test takes the
	// authenticated branch on a developer's machine and the unauthenticated one on CI,
	// and writes their real token into a file on the way.
	cmd := exec.Command("bash", "install.sh")
	cmd.Env = []string{
		"HOME=" + t.TempDir(),
		"PATH=" + ghStub(t, "") + ":" + pathPrefix + ":/usr/bin:/bin",
		"GITHUB_TOKEN=", "GH_TOKEN=",
		"SYNTY_INSTALL_API=http://127.0.0.1:1",
		"SYNTY_INSTALL_DOWNLOAD=http://127.0.0.1:1",
	}
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// The forward loop above iterates release.yml's own array, so it cannot see a platform
// the workflow stopped publishing: the loop just gets shorter and stays green while
// install.sh goes on deriving a label for it, and every fresh install there dies on a
// missing asset blaming the release rather than the installer. selfupdate's guard
// carries this reverse half; the installer had only the forward one. Drive uname pairs
// the workflow does not build and hold the script to refusing them or naming a label
// it publishes.
func TestInstallerClaimsNoPlatformTheReleaseDoesNotBuild(t *testing.T) {
	published := map[string]bool{}
	for _, p := range releasePlatforms(t) {
		published[p.Label] = true
	}
	announced := regexp.MustCompile(`INFO: platform: (\S+)`)
	for _, sysname := range []string{"Darwin", "Linux", "FreeBSD", "SunOS"} {
		for _, machine := range []string{"x86_64", "amd64", "arm64", "aarch64", "armv7l", "i386", "riscv64"} {
			t.Run(sysname+"/"+machine, func(t *testing.T) {
				out := runInstallerAs(t, unameStub(t, sysname, machine))
				m := announced.FindStringSubmatch(out)
				if m == nil {
					if !strings.Contains(out, "ERROR: unsupported") {
						t.Errorf("install.sh neither named a platform nor refused %s/%s:\n%s", sysname, machine, out)
					}
					return
				}
				if !published[m[1]] {
					t.Errorf("install.sh derives %q for uname -s %q -m %q, but release.yml publishes no such asset",
						m[1], sysname, machine)
				}
			})
		}
	}
}

// The linker does not fail over an -X symbol it cannot find, so renaming or
// relocating main.version would publish a whole release of binaries that report "dev"
// — which selfupdate refuses to update from — with every check still green. This test
// lives in package main so referencing the variable compiles only while it exists.
func TestReleaseStampsTheVersionVariableThisPackageDeclares(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release.yml: %v", err)
	}
	const want = "-X main.version="
	if !strings.Contains(string(raw), want) {
		t.Errorf("release.yml does not stamp %q; a release would ship binaries reporting %q", want, version)
	}
	// Taking the address is the part that cannot be faked: -X applies to a string
	// variable and is silently ignored for a constant, so `_ = version` compiles for
	// either while only this stops compiling the moment main.version stops being
	// settable. A const would otherwise ship a whole release reporting "dev".
	_ = &version

	// The workflow runs the binary it just built and reads the version back, because the
	// two checks above are both static: neither can see a -X that was accepted and did
	// nothing.
	if !strings.Contains(string(raw), `"dist/$bin" version`) {
		t.Error("release.yml no longer asks a built binary what version it reports; an -X that did not apply would ship")
	}
}

// install.sh reconstructs the whole asset filename and greps for it literally, while
// selfupdate matches only the suffix. The platform guards bind the label but not the
// name it sits in, so a change to the zip template keeps `update` working for everyone
// who already has a binary while every new install fails.
func TestInstallerAndWorkflowAgreeOnTheAssetFilename(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release.yml: %v", err)
	}
	// The zip's member name matters as much as the archive's. Both readers look for an
	// entry named exactly "synty-sync" (or synty-sync.exe): selfupdate.extractBinary
	// matches it outright, and install.sh unzips that path. Zipping "dist/$bin" instead
	// of cd-ing in first keeps this archive name intact while making every entry
	// "dist/synty-sync", which breaks the update and the install for every user with
	// nothing here to notice, so pin the whole command, not just its first argument.
	const template = `(cd dist && zip "synty-sync-${VERSION}-${label}.zip" "$bin"`
	if !strings.Contains(string(raw), template) {
		t.Errorf("release.yml no longer builds the asset with %s; both readers want a bare %q entry at the zip root",
			template, "synty-sync")
	}
	// And what "$bin" holds, which the template above cannot see. Both readers match the
	// entry name exactly — selfupdate.extractBinary against binaryName (+ ".exe" on
	// Windows) and install.sh against ${BINARY_NAME} — so renaming it here publishes an
	// archive neither can open. Nothing else would notice: the label guards compare
	// labels, install.sh never runs on Windows, and there is no Windows job in CI, so
	// the .exe arm would fail first and only for users of the one platform nothing here
	// executes.
	for _, assign := range []string{
		"bin=\"synty-sync\"",
		"if [ \"$goos\" = \"windows\" ]; then bin=\"synty-sync.exe\"; fi",
	} {
		if !strings.Contains(string(raw), assign) {
			t.Errorf("release.yml no longer contains %s; selfupdate.extractBinary and install.sh both match the zip entry by exact name", assign)
		}
	}
	sh, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	const composed = `local file="${BINARY_NAME}-${VERSION}-${PLATFORM}.zip"`
	if !strings.Contains(string(sh), composed) {
		t.Errorf("install.sh no longer composes %s; release.yml publishes synty-sync-<v>-<label>.zip", composed)
	}
}

// A tag can be repointed at any commit without anything here changing. The release job
// holds contents: write and publishes the binaries `update` installs unattended, and the
// CI job decides whether a tag ships at all, so every action in either one is pinned to
// a commit, first-party included: who wrote the action does not change what the job can
// do. The comment beside each pin names the exact release that commit is, since a
// floating major cannot answer which code a pin runs.
func TestEveryActionIsPinnedToACommit(t *testing.T) {
	// A real uses: value is a local path or owner/repo@ref. Anchoring on that shape keeps
	// prose that mentions the word out of the count.
	const action = `uses:[ \t]*(\./[A-Za-z0-9._/-]*|[A-Za-z0-9._-]+/[A-Za-z0-9._/-]+@[^\s#]+)`
	loose := regexp.MustCompile(action)
	strict := regexp.MustCompile(`^[ \t]*(?:-[ \t]+)?` + action + `[ \t]*(#.*)?$`)
	pinned := regexp.MustCompile(`@[0-9a-f]{40}$`)
	release := regexp.MustCompile(`^# v\d+\.\d+\.\d+$`)
	comment := regexp.MustCompile(`#.*$`)

	// Every workflow and local action under .github, not a named pair: a composite action
	// referenced as ./.github/actions/<name> runs its own uses: lines in the same job.
	var files []string
	err := filepath.WalkDir(".github", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ext := filepath.Ext(p); !d.IsDir() && (ext == ".yml" || ext == ".yaml") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	examined := 0
	for _, name := range files {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			m := strict.FindStringSubmatch(line)
			if m == nil {
				// `- {uses: actions/checkout@v6}` is legal YAML that GitHub runs, and an
				// anchored match alone cannot see it.
				if loose.MatchString(comment.ReplaceAllString(line, "")) {
					t.Errorf("%s:%d spells a uses: where this guard cannot check it; put it at the start of its own line: %s",
						name, i+1, strings.TrimSpace(line))
				}
				continue
			}
			examined++
			ref, note := m[1], strings.TrimSpace(m[2])
			if strings.HasPrefix(ref, "./") {
				continue // this repo's own workflow or action
			}
			if !pinned.MatchString(ref) {
				t.Errorf("%s:%d uses %s, which is not pinned to a 40-character commit sha", name, i+1, ref)
				continue
			}
			if !release.MatchString(note) {
				t.Errorf("%s:%d pins %s with comment %q; name the exact release that commit is, as # vX.Y.Z",
					name, i+1, ref, note)
			}
		}
	}
	// "Found nothing wrong" and "found nothing at all" must not be the same green.
	if examined < 5 {
		t.Errorf("examined %d uses: lines across %v; the guard is no longer reading the workflows", examined, files)
	}
}

// The token goes in a curl config file rather than curl's argv, where any other
// account on the machine could read it out of ps while an install is in flight. The
// file is created outside the staging directory, so the trap has to remove it too.
func TestInstallerKeepsTheTokenOutOfArgvAndLeavesNoConfigBehind(t *testing.T) {
	want := append(nativeMagic(t), []byte("a real enough binary")...)
	home := t.TempDir()
	srv := stubRelease(t, installerZip(t, want))
	tmp := t.TempDir()

	out, _ := runInstaller(t, home, "GITHUB_TOKEN=test-token", "TMPDIR="+tmp,
		"SYNTY_INSTALL_API="+srv.URL, "SYNTY_INSTALL_DOWNLOAD="+srv.URL)
	if _, err := os.ReadFile(filepath.Join(home, ".local", "bin", "synty-sync")); err != nil {
		t.Fatalf("nothing was installed: %v\n%s", err, out)
	}
	// TMPDIR is a directory only install.sh writes into, so the assertion is that it is
	// empty rather than that nothing in it wears the auth file's prefix: that prefix is
	// a mktemp template in install.sh, and a scan for it stops checking anything at all
	// the moment the template changes.
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("install.sh left %v in its TMPDIR; the token travels in one of those files", names)
	}
	// install.sh must not pass the token as an argument.
	sh, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sh), `-H "$hdr"`) || strings.Contains(string(sh), "Authorization: token $token") {
		t.Error("install.sh passes the Authorization header on curl's command line")
	}
}

// Everything else release.yml asserts about itself is pinned somewhere — the action's
// commit sha, the -X target, the asset name, the platform labels — but the three facts
// that decide whether a tag can ship untested code had no reader at all. Dropping
// "needs: test", inlining the checks instead of calling ci.yml, or hard-coding a Go
// version beside go-version-file all leave the whole suite green, and the next tag
// publishes binaries that never ran -race or the cross-compile step.
func TestReleaseRunsTheSameGateAMergeDoes(t *testing.T) {
	release, err := os.ReadFile(filepath.Join(".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release.yml: %v", err)
	}
	ci, err := os.ReadFile(filepath.Join(".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}

	// The gate is CI itself, called, not a second copy of the checks that can drift.
	if !strings.Contains(string(release), "uses: ./.github/workflows/ci.yml") {
		t.Error("release.yml no longer calls ci.yml as its gate")
	}
	if !strings.Contains(string(ci), "workflow_call:") {
		t.Error("ci.yml no longer accepts workflow_call, so release.yml cannot use it as its gate")
	}
	// And the publishing job waits on it.
	publishes := regexp.MustCompile(`(?s)\n  release:\n(.*?)(\n  \w|\z)`).FindSubmatch(release)
	if publishes == nil {
		t.Fatal("no release job found in release.yml")
	}
	if !strings.Contains(string(publishes[1]), "needs: test") {
		t.Error("the release job no longer waits on the test job; a tag could publish untested code")
	}
	if regexp.MustCompile(`continue-on-error:\s*true`).Match(release) {
		t.Error("release.yml has a continue-on-error, which can let a failed gate through")
	}

	// And the gate itself waits on the branch check. A tag is pushed by hand and can
	// name any commit in the repo, so without this a `git tag v1.2.3 <sha-on-a-branch>`
	// publishes binaries from code that never landed on main: tested, but not what main
	// carries and not what anyone reviewed. Dropping either the job or the needs leaves
	// every other assertion here green.
	if !regexp.MustCompile(`(?m)^  check-branch:$`).Match(release) {
		t.Error("release.yml no longer has a check-branch job; a tag on an unmerged branch could publish")
	}
	if !strings.Contains(string(release), "git merge-base --is-ancestor") {
		t.Error("check-branch no longer proves the tagged commit is an ancestor of main")
	}
	gates := regexp.MustCompile(`(?s)\n  test:\n(.*?)(\n  \w|\z)`).FindSubmatch(release)
	if gates == nil {
		t.Fatal("no test job found in release.yml")
	}
	if !strings.Contains(string(gates[1]), "needs: check-branch") {
		t.Error("the test job no longer waits on check-branch; a tag off main could reach the release job")
	}

	// go.mod is the single source of the toolchain version in both files.
	bare := regexp.MustCompile(`go-version:\s`)
	for name, raw := range map[string][]byte{"release.yml": release, "ci.yml": ci} {
		if bare.Match(raw) {
			t.Errorf("%s pins a go-version directly; go-version-file: go.mod is the source", name)
		}
		if !strings.Contains(string(raw), "go-version-file: go.mod") {
			t.Errorf("%s no longer reads the Go version from go.mod", name)
		}
	}

	// Write is granted to the one job that publishes, never at workflow level, where
	// every job including the gate would inherit it.
	for name, raw := range map[string][]byte{"release.yml": release, "ci.yml": ci} {
		head, _, _ := strings.Cut(string(raw), "\njobs:")
		if !strings.Contains(head, "permissions:\n  contents: read") {
			t.Errorf("%s does not default to contents: read above its jobs", name)
		}
		if strings.Contains(head, "contents: write") {
			t.Errorf("%s grants contents: write at workflow level; only the release job needs it", name)
		}
	}
	if strings.Contains(string(ci), "contents: write") {
		t.Error("ci.yml grants contents: write; it only ever reads the repo")
	}
}

// GitHub pretty-prints its API responses today, and that is not a contract. The
// installer reads both the release tag and the asset URL out of the payload by text, so
// a payload that arrives on one line has to read the same. A greedy match over an
// unstripped document takes the last quoted run in the whole thing — the release body,
// or the final asset's download URL — which is not a version, and the installer then
// fails with "asset … not found in release v<that>", pointing at the release rather
// than at its own parse.
func TestInstallerReadsTheReleaseHoweverGitHubSpacesIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		compact bool
	}{
		{"pretty-printed, as the API sends it today", false},
		{"on one line", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := append(nativeMagic(t), []byte("a real enough binary")...)
			home := t.TempDir()
			srv := stubReleaseShaped(t, installerZip(t, want), assetShape{compact: tc.compact})

			// No version argument, so this goes through latest_version rather than
			// taking the tag from the command line.
			out, _ := runInstaller(t, home, "GITHUB_TOKEN="+stubToken,
				"SYNTY_INSTALL_API="+srv.URL, "SYNTY_INSTALL_DOWNLOAD="+srv.URL)

			got, err := os.ReadFile(filepath.Join(home, ".local", "bin", "synty-sync"))
			if err != nil {
				t.Fatalf("nothing was installed: %v\n%s", err, out)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("installed %d bytes, want the %d from the asset", len(got), len(want))
			}
		})
	}
}

// install.sh ends on a smoke test because err alone returns 0, so without it anything
// piping the script would read a broken install as a successful one. Only the failing
// direction is covered: every other test here ships an asset that cannot execute, so a
// non-zero exit is expected and thrown away, and a regression leaving the script
// non-zero after a correct install would pass all of them. Ship one real executable.
func TestInstallerExitsZeroOnASuccessfulInstall(t *testing.T) {
	// A real native binary that exits 0 for any argv, so the smoke test can actually
	// run the thing that was installed.
	path, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true(1) on disk to stand in for a working binary")
	}
	bin, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("cannot read %s: %v", path, err)
	}

	home := t.TempDir()
	srv := stubRelease(t, installerZip(t, bin))
	out, err := runInstaller(t, home, "GITHUB_TOKEN=test-token",
		"SYNTY_INSTALL_API="+srv.URL, "SYNTY_INSTALL_DOWNLOAD="+srv.URL)
	if err != nil {
		t.Fatalf("install.sh exited non-zero after installing a binary that runs: %v\n%s", err, out)
	}
	if !strings.Contains(out, "installed to") {
		t.Errorf("install.sh exited 0 without reporting an install:\n%s", out)
	}
}
