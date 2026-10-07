package server

import (
	"errors"
	"testing"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
)

// The run-files listing drops the canonical tree-noise entries: they are
// the engine's own writes, not changes the workflow produced. An error
// passes through untouched.
func TestNoiseFreeDropsTheCanonicalNoise(t *testing.T) {
	files := []gitlib.FileStatus{
		{Path: ".claude/skills/x.md"},
		{Path: "devbox.lock"},
		{Path: ".iterion-script-a1.sh"},
		{Path: "main.go"},
	}
	got, err := noiseFree(files, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "main.go" {
		t.Fatalf("the noise survived the listing: %+v", got)
	}
	sentinel := errors.New("git said no")
	if _, err := noiseFree(nil, sentinel); !errors.Is(err, sentinel) {
		t.Fatalf("the error did not pass through: %v", err)
	}
}
