package selfupdate

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

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
	published := map[string]bool{}
	for _, p := range built {
		published[p.label] = true
	}

	rel := &release{}
	for _, p := range built {
		rel.Assets = append(rel.Assets, asset{Name: "synty-sync-1.0.0-" + p.label + ".zip", URL: "u/" + p.label})
	}

	// Forward: every platform the workflow builds resolves to that platform's asset.
	for _, p := range built {
		t.Run(p.goos+"/"+p.goarch, func(t *testing.T) {
			label, err := assetSuffix(p.goos, p.goarch)
			if err != nil {
				t.Fatalf("release.yml builds %s/%s but the updater has no label for it: %v", p.goos, p.goarch, err)
			}
			if label != p.label {
				t.Errorf("the updater wants %q, but release.yml publishes %q", label, p.label)
			}
			url, err := platformAsset(rel, p.goos, p.goarch)
			if err != nil {
				t.Fatalf("release.yml builds %s/%s but the updater cannot find it: %v", p.goos, p.goarch, err)
			}
			if url != "u/"+p.label {
				t.Errorf("platformAsset = %q, want the %q asset release.yml publishes", url, p.label)
			}
		})
	}

	// Reverse: every label the updater asks for is one the workflow publishes. This is
	// what catches a platform removed from the array, which the forward loop cannot see
	// because it iterates that same array.
	for _, goos := range []string{"darwin", "linux", "windows"} {
		for _, goarch := range []string{"amd64", "arm64"} {
			label, err := assetSuffix(goos, goarch)
			if err != nil {
				continue // the updater does not claim this platform
			}
			if !published[label] {
				t.Errorf("the updater resolves %s/%s to %q, which release.yml does not build; "+
					"`update` on that platform reports no asset for it", goos, goarch, label)
			}
		}
	}

	if _, err := assetSuffix("plan9", "amd64"); err == nil {
		t.Error("an unsupported platform resolved to a label")
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
