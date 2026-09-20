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
