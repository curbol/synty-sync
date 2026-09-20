package releaseyml

import (
	"os"
	"path/filepath"
	"testing"
)

func workflow(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", ".github", "workflows", "release.yml")
}

func TestPlatformsReadsTheRealWorkflow(t *testing.T) {
	got, err := Platforms(workflow(t))
	if err != nil {
		t.Fatalf("Platforms: %v", err)
	}
	seen := map[string]bool{}
	for _, p := range got {
		if p.GOOS == "" || p.GOARCH == "" || p.Label == "" {
			t.Errorf("incomplete entry %+v", p)
		}
		seen[p.Label] = true
	}
	// The labels the installer and the updater both resolve to. Naming them here would
	// be a second source of truth if this were the only check, but it is not: both
	// guards read the same file, and this one only asserts the parser reaches them.
	for _, label := range []string{"mac-intel", "mac-apple", "linux-intel", "linux-arm64", "win"} {
		if !seen[label] {
			t.Errorf("the workflow's platform list no longer yields %q", label)
		}
	}
}

// The shape guards are the whole value of the parser: a workflow it cannot read has to
// fail its callers rather than hand them an empty list, which every guard built on it
// would pass vacuously.
func TestAnUnreadableShapeIsAnErrorNotAnEmptyList(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no platforms array", "jobs:\n  release:\n    steps: []\n"},
		{"unterminated array", "platforms=(\n  \"linux/amd64/linux-intel\"\n"},
		{"array with no entries", "platforms=(\n)\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "release.yml")
			if err := os.WriteFile(p, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := Platforms(p)
			if err == nil {
				t.Fatalf("parsed %d platforms out of an unreadable shape, want an error: %+v", len(got), got)
			}
		})
	}
}

func TestAMissingWorkflowIsAnError(t *testing.T) {
	if _, err := Platforms(filepath.Join(t.TempDir(), "absent.yml")); err == nil {
		t.Fatal("a missing workflow parsed without error")
	}
}
