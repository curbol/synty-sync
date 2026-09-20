package selfupdate

import (
	"path/filepath"
	"testing"

	"github.com/curbol/synty-sync/internal/releaseyml"
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
		published[p.Label] = true
	}

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
