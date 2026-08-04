package selfupdate

import (
	"runtime"
	"strings"
	"testing"
)

func TestPlatformAssetSelectsCurrentOS(t *testing.T) {
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

	url, err := platformAsset(rel)
	if err != nil {
		t.Fatalf("platformAsset: %v", err)
	}

	want := map[string]string{
		"darwin":  map[string]string{"amd64": "u/mac-intel", "arm64": "u/mac-apple"}[runtime.GOARCH],
		"linux":   map[string]string{"amd64": "u/linux-intel", "arm64": "u/linux-arm64"}[runtime.GOARCH],
		"windows": "u/win",
	}[runtime.GOOS]
	if want != "" && url != want {
		t.Errorf("platformAsset = %q, want %q for %s/%s", url, want, runtime.GOOS, runtime.GOARCH)
	}
}

func TestPlatformAssetMissing(t *testing.T) {
	rel := &release{Assets: []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}{
		{Name: "synty-sync-1.0.0-solaris-sparc.zip", URL: "u/nope"},
	}}
	if _, err := platformAsset(rel); err == nil || !strings.Contains(err.Error(), "no asset matching") {
		t.Errorf("expected no-asset error, got %v", err)
	}
}
