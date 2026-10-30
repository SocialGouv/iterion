package ir

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The ir-leaf claim, checked WITHOUT internal/layerguard: the guard's rules
// bind DIRECT imports by decision, so a banned family reached through an
// accepted vocabulary package is no hit there — which is exactly how reading
// seven boardops.CapBoard* constants once linked the native dispatcher, the
// store, the forge and the mailer into every binary that compiles a .bot
// (iterion#2857). This witness asks `go list -deps` instead: the transitive
// closure the linker actually ships.
//
// The forbidden set is the weight #2857 removed, named so its return is a
// red a reviewer reads: the dispatcher and the forge are on the leaf rule's
// banned list already (one hop is all the guard needs for those); the mailer
// and the claw runtime are the transitive drag no direct import ever names.
// pkg/store is NOT named: the IR imports pkg/knowledge and pkg/runops
// directly, and both legitimately reach it.
var theLeafClosureNeverLinks = []string{
	"github.com/SocialGouv/iterion/pkg/dispatcher",
	"github.com/SocialGouv/iterion/pkg/forge",
	"github.com/SocialGouv/iterion/pkg/mail",
	"github.com/SocialGouv/claw-code-go",
}

func TestTheLeafClosureLinksNoDispatcherForgeMailerOrClawRuntime(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := ""
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			root = d
			break
		}
		if filepath.Dir(d) == d {
			t.Fatal("no go.mod above the test — run from inside the module")
		}
	}
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "./pkg/dsl/ir")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps ./pkg/dsl/ir failed: %v — an empty closure certifies nothing", err)
	}
	closure := strings.Split(string(out), "\n")
	if len(closure) < 2 {
		t.Fatalf("go list -deps ./pkg/dsl/ir named %d package(s) — an empty closure certifies nothing", len(closure))
	}
	for _, pkg := range closure {
		for _, forbidden := range theLeafClosureNeverLinks {
			if pkg == forbidden || strings.HasPrefix(pkg, forbidden+"/") {
				t.Errorf("pkg/dsl/ir's transitive closure links %s — the leaf claim is transitive, and %s is the weight iterion#2857 removed: the capability vocabulary the IR validates against must live in a leaf (pkg/boardcaps, the backendcaps registry), never inside the implementation that reads it", pkg, forbidden)
			}
		}
	}
}
