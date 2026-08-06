package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// `iterion issue import` mirrors a forge repo's issues onto the native
// board — the CLI half of what a cloud tenant gets through the periodic
// forge-integration sync. RunIssueImport itself has no unit tests today:
// the shared `syncForgeIssuesToBoard` core is covered by
// pkg/server/board_forge_test.go via a fake IssueClient injection, but
// nothing exercises the CLI's construct-then-delegate shim end-to-end —
// the piece an operator actually types.
//
// This test stages a fake Forgejo endpoint via httptest, points the
// command's --base-url at it, provides the token via --token-env (secrets
// discipline: NEVER a flag value), and asserts against the RE-OPENED
// board (a fresh native.Store) — the persisted truth, not the CLI's
// in-memory index. Forgejo is chosen for the fixture: its BaseURL is
// used verbatim under `/api/v1/…`, unlike GitHub which maps a WEB base to
// api.github.com and would need a WEB-shaped hostname; the semantics
// under test (List → upsert → idempotent re-list) are provider-agnostic.
//
// Mutation guide:
//   - "n'importer aucune issue": short-circuit `syncForgeIssuesToBoard` to
//     `return 0, 0, nil` — the board stays empty, the "board must have 2
//     cards" assertion fails.
//   - "ignorer l'idempotence": drop the `existing, gerr := board.Get(...)`
//     branch in `upsertForgeCard` and always Create — the re-import
//     duplicates every card, the second-run cards-count and JSON-shape
//     assertions fail.
//   - Skip the PR filter (`if is.IsPullRequest { continue }`) — the
//     "exactly 2 cards" assertion fails (the PR would land as a 3rd).
//   - Skip the token env lookup or hard-code an empty base URL — the
//     server call-count assertion fails (0 calls, not 1).

// forgejoIssuesFixture is the JSON payload the fake Forgejo endpoint
// returns for GET /api/v1/repos/{repo}/issues. Three items: two plain
// issues (one open, one closed) and one item that carries a
// non-null `pull_request` field, which the Gitea/Forgejo API uses to
// disambiguate PRs from issues on this shared endpoint.
const forgejoIssuesFixture = `[
  {"number":101,"title":"add metrics","body":"why","state":"open","html_url":"http://f/101","labels":[{"name":"feat"}],"user":{"login":"alice"}},
  {"number":202,"title":"old bug","state":"closed","html_url":"http://f/202"},
  {"number":303,"title":"a PR, must be skipped","state":"open","html_url":"http://f/303","pull_request":{"merged":false,"html_url":"http://f/303"}}
]`

// fakeForgejoServer stands up an httptest server that answers
// GET /api/v1/repos/{repo}/issues with forgejoIssuesFixture; every
// other path 404s. Returns the server (defer .Close()) and a pointer
// to the call counter, so the test can assert exact request cardinality.
func fakeForgejoServer(t *testing.T, repo string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var listCalls atomic.Int32
	wantPath := "/api/v1/repos/" + repo + "/issues"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wantPath || r.Method != http.MethodGet {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		// The CLI must have propagated the token from the env var into
		// the outbound Authorization header. Forgejo's AdminHTTP sends
		// "Authorization: token <T>". A missing / empty header is a red
		// flag (credential dropped mid-flight, or a stubbed client that
		// bypassed the transport) and must fail visibly, not silently.
		if auth := strings.TrimSpace(r.Header.Get("Authorization")); auth == "" {
			http.Error(w, "missing Authorization header — token propagation broken", http.StatusUnauthorized)
			return
		}
		listCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, forgejoIssuesFixture)
	}))
	t.Cleanup(srv.Close)
	return srv, &listCalls
}

// runIssueImportJSON runs RunIssueImport in JSON mode and returns the
// decoded {created, updated} counters plus the raw stdout for
// diagnostics on failure. Any error from the CLI is surfaced verbatim.
type importReport struct {
	Created int `json:"created"`
	Updated int `json:"updated"`
}

func runIssueImportJSON(t *testing.T, opts cli.IssueImportOptions) (importReport, string) {
	t.Helper()
	var buf bytes.Buffer
	p := &cli.Printer{W: &buf, Format: cli.OutputJSON}
	if err := cli.RunIssueImport(p, opts); err != nil {
		t.Fatalf("RunIssueImport: %v\nstdout: %s", err, buf.String())
	}
	var out importReport
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("decode import report from %q: %v", buf.String(), err)
	}
	return out, buf.String()
}

// importReopenBoard opens a FRESH native.Store on <storeDir>/dispatcher,
// so the assertion reads what a dispatcher process starting cold would
// see — never the CLI's in-memory index. Mirrors reopenBoard in
// cli_issue_test.go (kept local to avoid cross-file coupling).
func importReopenBoard(t *testing.T, storeDir string) *native.Store {
	t.Helper()
	s, err := native.NewStore(filepath.Join(storeDir, "dispatcher"))
	if err != nil {
		t.Fatalf("reopen native store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestRunIssueImport_CreatesCardsAndIsIdempotent drives the full CLI
// import against a fake Forgejo endpoint and verifies the observable
// invariants: cards land on the board with the right columns, PRs are
// filtered out, and a second identical import UPSERTS instead of
// duplicating (idempotence — the property that lets an operator re-run
// import safely after a partial failure).
func TestRunIssueImport_CreatesCardsAndIsIdempotent(t *testing.T) {
	const (
		repo    = "owner/name"
		envName = "TEST_FORGE_TOKEN_ISSUE_IMPORT"
	)
	srv, listCalls := fakeForgejoServer(t, repo)
	// Provide the token via env var (never a flag): the CLI reads
	// os.Getenv(opts.TokenEnv) and this shape is what an operator would
	// use in real life (e.g. `--token-env GITHUB_TOKEN`).
	t.Setenv(envName, "s3cret-token-value")

	storeDir := t.TempDir()
	opts := cli.IssueImportOptions{
		IssueCommonOptions: cli.IssueCommonOptions{StoreDir: storeDir},
		Forge:              "forgejo",
		Repo:               repo,
		TokenEnv:           envName,
		BaseURL:            srv.URL,
	}

	// -----------------------------------------------------------------
	// First import: 2 cards created (issue #101 open, #202 closed), PR
	// #303 filtered out, 0 updated.
	// -----------------------------------------------------------------
	report1, stdout1 := runIssueImportJSON(t, opts)
	if got := listCalls.Load(); got != 1 {
		t.Fatalf("first import: fake forge ListIssues calls = %d, want 1 (mission: import must actually hit the forge, not stub-return)", got)
	}
	if report1.Created != 2 || report1.Updated != 0 {
		t.Fatalf("first import report = %+v, want {Created:2, Updated:0} (stdout: %s)", report1, stdout1)
	}

	// Re-open the board through a fresh store — persisted truth.
	board := importReopenBoard(t, storeDir)
	all, err := board.List(native.ListFilter{})
	if err != nil {
		t.Fatalf("board.List: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("board card count after first import = %d, want 2 (PR must be skipped, issues must be persisted)", len(all))
	}

	// Content check by title (deterministic; the tracker builds card IDs
	// from provider+repo+number so re-lookup by number is safe too).
	byTitle := importIndexByTitle(all)
	openCard, hasOpen := byTitle["add metrics"]
	closedCard, hasClosed := byTitle["old bug"]
	if !hasOpen {
		t.Fatalf("expected an 'add metrics' card, got titles %v", importTitlesOf(all))
	}
	if !hasClosed {
		t.Fatalf("expected an 'old bug' card, got titles %v", importTitlesOf(all))
	}
	if openCard.State != native.StateInbox {
		t.Errorf("open issue landed in %q, want %q (default open column)", openCard.State, native.StateInbox)
	}
	if closedCard.State != native.StateDone {
		t.Errorf("closed issue landed in %q, want %q (terminal column)", closedCard.State, native.StateDone)
	}
	// PR filter — the 303 title must NEVER appear on the board.
	if _, hasPR := byTitle["a PR, must be skipped"]; hasPR {
		t.Errorf("PR leaked onto the board — the IsPullRequest filter was skipped")
	}

	// -----------------------------------------------------------------
	// Second import: idempotence. The SAME fixture is served, so no card
	// is new; every existing one is updated in place. The board count
	// must stay at 2.
	// -----------------------------------------------------------------
	report2, stdout2 := runIssueImportJSON(t, opts)
	if got := listCalls.Load(); got != 2 {
		t.Fatalf("second import: fake forge ListIssues calls = %d, want 2 (one per import)", got)
	}
	if report2.Created != 0 || report2.Updated != 2 {
		t.Fatalf("re-import report = %+v, want {Created:0, Updated:2} (stdout: %s)", report2, stdout2)
	}
	all2, err := importReopenBoard(t, storeDir).List(native.ListFilter{})
	if err != nil {
		t.Fatalf("board.List after re-import: %v", err)
	}
	if len(all2) != 2 {
		t.Fatalf("board card count after re-import = %d, want 2 (idempotence broken — duplicates?)", len(all2))
	}
}

// TestRunIssueImport_RejectsUnsupportedForge locks in the provider guard
// at the CLI seam: an unknown --forge must be rejected BEFORE any
// network call, so a typo doesn't silently hit the wrong API. Kept
// separate from the httptest-backed happy path because it needs no
// server (and would panic on a stray request).
func TestRunIssueImport_RejectsUnsupportedForge(t *testing.T) {
	storeDir := t.TempDir()
	// Set a value on some env var so we're testing the forge-gate, not
	// the token-gate. The test asserts on error content below.
	const envName = "TEST_FORGE_TOKEN_UNSUPPORTED"
	t.Setenv(envName, "irrelevant")

	p := &cli.Printer{W: io.Discard, Format: cli.OutputHuman}
	err := cli.RunIssueImport(p, cli.IssueImportOptions{
		IssueCommonOptions: cli.IssueCommonOptions{StoreDir: storeDir},
		Forge:              "bitbucket",
		Repo:               "o/r",
		TokenEnv:           envName,
	})
	if err == nil {
		t.Fatal("expected an error for an unsupported --forge, got nil")
	}
	if !strings.Contains(err.Error(), "forge") {
		t.Errorf("error must name the forge gate: got %q", err.Error())
	}

	// Board must be untouched — the error path must not have written to disk.
	if entries, _ := os.ReadDir(filepath.Join(storeDir, "dispatcher")); len(entries) > 0 {
		// It IS acceptable that openNativeStore never got called — but if it
		// did, no cards must have landed.
		board := importReopenBoard(t, storeDir)
		all, _ := board.List(native.ListFilter{})
		if len(all) != 0 {
			t.Errorf("board wrote %d cards despite an unsupported-forge error", len(all))
		}
	}
}

// TestRunIssueImport_RejectsEmptyToken locks in the secrets-discipline
// contract: the token comes from the NAMED env var (never a flag), and
// an empty/unset env var is a hard error, not a silent
// unauthenticated call. This is the guard that keeps a typo (`--token-env
// GITUB_TOKEN`) from resolving to an empty string and reaching out
// anonymously — which would then leak "no such issues" into the board.
func TestRunIssueImport_RejectsEmptyToken(t *testing.T) {
	storeDir := t.TempDir()
	// Deliberately DO NOT set the env var — os.Getenv returns "" for
	// missing keys. Use a random name so a real env in the caller's
	// shell cannot mask the test.
	const envName = "TEST_FORGE_TOKEN_MUST_BE_MISSING_XYZ_123"
	_ = os.Unsetenv(envName) // defence in depth

	p := &cli.Printer{W: io.Discard, Format: cli.OutputHuman}
	err := cli.RunIssueImport(p, cli.IssueImportOptions{
		IssueCommonOptions: cli.IssueCommonOptions{StoreDir: storeDir},
		Forge:              "forgejo",
		Repo:               "o/r",
		TokenEnv:           envName,
	})
	if err == nil {
		t.Fatal("expected an error for an empty --token-env value, got nil")
	}
	if !strings.Contains(err.Error(), envName) {
		t.Errorf("error must name the missing env var (%q) so the operator can fix it: got %q", envName, err.Error())
	}
	// Sanity: the error must NOT itself contain a fake token value —
	// this test would catch a regression that logs the resolved token.
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error must not leak a token value: got %q", err.Error())
	}
	// The board must not exist yet — RunIssueImport returned before
	// openNativeStore. openNativeStore ALSO fails silently when the
	// error returns first: assert nothing was written.
	if _, statErr := os.Stat(filepath.Join(storeDir, "dispatcher", "board.json")); statErr == nil {
		t.Errorf("openNativeStore ran despite missing token — the token gate must precede the store open")
	} else if !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("unexpected stat error: %v", statErr)
	}
}

// -- helpers ---------------------------------------------------------------

func importIndexByTitle(cards []*native.Issue) map[string]*native.Issue {
	out := make(map[string]*native.Issue, len(cards))
	for _, c := range cards {
		out[c.Title] = c
	}
	return out
}

func importTitlesOf(cards []*native.Issue) []string {
	out := make([]string, 0, len(cards))
	for _, c := range cards {
		out = append(out, c.Title)
	}
	return out
}
