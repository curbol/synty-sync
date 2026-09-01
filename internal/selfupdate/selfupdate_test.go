package selfupdate

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// Every platform's suffix has to match a label in release.yml, and the workflow is
// the only place those labels are really decided. Asserting platformAsset against a
// list this test writes down would just be a fourth copy of them: renaming a label
// in the workflow would leave every check green and ship "no asset for your
// platform" to the one platform CI does not run on. So read the workflow.
func TestPlatformAssetMatchesTheLabelsReleaseBuilds(t *testing.T) {
	built := releasePlatforms(t)
	rel := &release{Assets: []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}{}}
	for _, p := range built {
		rel.Assets = append(rel.Assets, struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		}{Name: "synty-sync-1.0.0-" + p.label + ".zip", URL: "u/" + p.label})
	}

	claimed := map[string]bool{}
	for _, p := range built {
		t.Run(p.goos+"/"+p.goarch, func(t *testing.T) {
			url, err := platformAsset(rel, p.goos, p.goarch)
			if err != nil {
				t.Fatalf("release.yml builds %s/%s but the updater cannot find it: %v", p.goos, p.goarch, err)
			}
			if url != "u/"+p.label {
				t.Errorf("platformAsset = %q, want the %q asset release.yml publishes", url, p.label)
			}
		})
		claimed[p.label] = true
	}
	// The other direction: a suffix the updater asks for that the workflow no longer
	// publishes fails the same way, and would otherwise go unnoticed until a user ran
	// update on that platform.
	for _, goos := range []string{"darwin", "linux", "windows"} {
		for _, goarch := range []string{"amd64", "arm64"} {
			url, err := platformAsset(rel, goos, goarch)
			if err != nil {
				continue // not built for this pair; the loop above covers what is
			}
			if !claimed[strings.TrimPrefix(url, "u/")] {
				t.Errorf("the updater resolves %s/%s to %q, which release.yml does not build", goos, goarch, url)
			}
		}
	}

	if _, err := platformAsset(rel, "plan9", "amd64"); err == nil {
		t.Error("an unsupported platform resolved to an asset")
	}
}

type releasePlatform struct{ goos, goarch, label string }

var platformEntryRe = regexp.MustCompile(`"([a-z0-9]+)/([a-z0-9]+)/([a-z0-9-]+)"`)

// releasePlatforms reads the platforms the release workflow actually builds, so the
// asset labels have exactly one source of truth.
func releasePlatforms(t *testing.T) []releasePlatform {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release.yml: %v", err)
	}
	_, rest, ok := strings.Cut(string(raw), "platforms=(")
	if !ok {
		t.Fatal("release.yml has no platforms=( ... ) list; this guard no longer reads what the workflow builds")
	}
	block, _, ok := strings.Cut(rest, ")")
	if !ok {
		t.Fatal("release.yml's platforms list is unterminated")
	}
	var out []releasePlatform
	for _, m := range platformEntryRe.FindAllStringSubmatch(block, -1) {
		out = append(out, releasePlatform{goos: m[1], goarch: m[2], label: m[3]})
	}
	if len(out) == 0 {
		t.Fatal("no platforms parsed from release.yml")
	}
	return out
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
