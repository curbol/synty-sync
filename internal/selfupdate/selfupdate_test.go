package selfupdate

import (
	"runtime"
	"strings"
	"testing"
)

// Every platform's suffix has to match a label in release.yml. Asserting only the
// one the test host happens to run on means CI (linux/amd64) never checks the
// others, and a typo there ships as "no asset for your platform".
func TestPlatformAssetSelectsThePlatformsTheReleaseBuilds(t *testing.T) {
	rel := &release{Assets: []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}{
		{Name: "synty-sync-1.0.0-mac-intel.zip", URL: "u/mac-intel"},
		{Name: "synty-sync-1.0.0-mac-apple.zip", URL: "u/mac-apple"},
		{Name: "synty-sync-1.0.0-linux-intel.zip", URL: "u/linux-intel"},
		{Name: "synty-sync-1.0.0-linux-arm64.zip", URL: "u/linux-arm64"},
		{Name: "synty-sync-1.0.0-win.zip", URL: "u/win"},
	}}

	for _, tc := range []struct {
		goos, goarch, want string
	}{
		{"darwin", "amd64", "u/mac-intel"},
		{"darwin", "arm64", "u/mac-apple"},
		{"linux", "amd64", "u/linux-intel"},
		{"linux", "arm64", "u/linux-arm64"},
		{"windows", "amd64", "u/win"},
	} {
		t.Run(tc.goos+"/"+tc.goarch, func(t *testing.T) {
			url, err := platformAsset(rel, tc.goos, tc.goarch)
			if err != nil {
				t.Fatalf("platformAsset: %v", err)
			}
			if url != tc.want {
				t.Errorf("platformAsset = %q, want %q", url, tc.want)
			}
		})
	}

	if _, err := platformAsset(rel, "plan9", "amd64"); err == nil {
		t.Error("an unsupported platform resolved to an asset")
	}
}

func TestPlatformAssetMissing(t *testing.T) {
	rel := &release{Assets: []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}{
		{Name: "synty-sync-1.0.0-solaris-sparc.zip", URL: "u/nope"},
	}}
	if _, err := platformAsset(rel, runtime.GOOS, runtime.GOARCH); err == nil || !strings.Contains(err.Error(), "no asset matching") {
		t.Errorf("expected no-asset error, got %v", err)
	}
}
