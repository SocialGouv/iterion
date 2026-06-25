//go:build live

package e2e

import (
	"os"
	"testing"
	"time"
)

// TestLive_Bot_AdrCartograph runs the adr-cartograph bot (Adry) against a
// small service that embodies undocumented architectural decisions
// (in-memory mutex-guarded storage, stdlib net/http, no DB). Adry scans
// for existing ADRs, surveys the code for decisions/gaps, and authors ADR
// markdown under docs/adr in a cross-family review loop.
//
// Store is isolated to the temp workspace (any handoff board issues stay
// contained) and loaded as a bundle so Adry's skills mirror.
//
// Reliability invariants: scan_adrs + survey_code fire, and the run
// produces ADRs (a commit beyond seed or files under docs/adr) or
// converges. Then the quality panel grades the ADRs + value.
//
// Requires: claude CLI + OpenAI. Expected: ~20-40 min.
func TestLive_Bot_AdrCartograph(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")
	requireOpenAI(t)
	t.Setenv("ITERION_TEST_STORE_DIR", "workspace") // isolate any handoff board issues

	workspaceDir, err := os.MkdirTemp("", "iterion-adr-cartograph-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Logf("Workspace (persists): %s", workspaceDir)

	seedCommits := seedGoModuleFixture(t, workspaceDir, map[string]string{
		"store.go": `package fixture

import "sync"

// Store is an in-memory key/value store guarded by a mutex. We chose
// in-memory over a database to keep the service dependency-free.
type Store struct {
	mu sync.Mutex
	m  map[string]string
}

func New() *Store { return &Store{m: map[string]string{}} }

func (s *Store) Put(k, v string) { s.mu.Lock(); defer s.mu.Unlock(); s.m[k] = v }
func (s *Store) Get(k string) string { s.mu.Lock(); defer s.mu.Unlock(); return s.m[k] }
`,
		"server.go": `package fixture

import "net/http"

// We serve with the standard library net/http rather than a framework.
func Handler(s *Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/get", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(s.Get(r.URL.Query().Get("k"))))
	})
	return mux
}
`,
		"README.md": "# fixture\n\nIn-memory mutex-guarded storage; stdlib net/http; no database.\n",
	})

	vars := map[string]interface{}{
		"workspace_dir":          workspaceDir,
		"rechallenge_after_days": 0, // no handoff issues
		"scope_notes":            "Document the storage and HTTP-framework decisions.",
	}
	res := runBotLive(t, liveSpec{
		runIDBase:    "live-adr-cartograph",
		bundleDir:    "../bots/adr-cartograph",
		workspaceDir: workspaceDir,
		vars:         vars,
		inputs:       vars,
		timeout:      40 * time.Minute,
		autoResume:   true, // defensive: survey/fix may escalate via ask_user
		maxResumes:   10,
	})

	assertNodesFinished(t, res.events, "scan_adrs", "survey_code")
	adrsWritten := len(gitOut(workspaceDir, "ls-files", "docs/adr")) > 0
	committed := workspaceCommitCount(t, workspaceDir) > seedCommits
	t.Logf("adr-cartograph outcome: adrFilesPresent=%v committed=%v", adrsWritten, committed)

	assessQuality(t, res, qualityInput{
		kind:          "bot",
		name:          "adr-cartograph",
		persona:       "Adry",
		primaryFamily: "anthropic",
		task:          "Cartograph undocumented decisions (in-memory storage, stdlib net/http) into ADRs under docs/adr.",
		workProduct:   gitArtifactEvidence(t, workspaceDir),
	})
}
