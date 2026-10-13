package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// `iterion issue import` streams each page onto the board as it arrives, so a
// read refused mid-window leaves cards behind: the pages that came are
// mirrored, the rest is not. The three counters are printed only on the
// success path, so a refusal has to carry them itself — an operator who cannot
// tell an import that wrote 20 000 cards from one that wrote none re-runs
// blind, and the command is the self-hosted entry point with no high-water
// mark and no next pass to clean up after it.
// The count is of ISSUES: the window below carries four entries and one of
// them is a pull request, which that endpoint serves and the sync discards
// before the board. The read's own refusals price a window in ENTRIES, so
// using one word for both sets would put two irreconcilable numbers in front
// of one operator.
func TestRunIssueImport_ARefusedWindowReportsWhatLanded(t *testing.T) {
	const landed, served = 3, 4
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/repos/{owner}/{name}/issues", func(w http.ResponseWriter, r *http.Request) {
		if page, _ := strconv.Atoi(r.URL.Query().Get("page")); page > 1 {
			http.Error(w, "the forge dropped the page", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[
  {"number":101,"title":"one","state":"open","html_url":"http://f/101"},
  {"number":102,"title":"two","state":"open","html_url":"http://f/102"},
  {"number":103,"title":"three","state":"open","html_url":"http://f/103"},
  {"number":104,"title":"a pull request","state":"open","html_url":"http://f/104","pull_request":{"url":"http://f/pulls/104"}}
]`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	storeDir := t.TempDir()
	t.Setenv("ITERION_TEST_FORGE_TOKEN", "tok")
	var out bytes.Buffer
	err := RunIssueImport(&Printer{W: &out, Format: OutputHuman}, IssueImportOptions{
		IssueCommonOptions: IssueCommonOptions{StoreDir: storeDir},
		Forge:              "forgejo",
		BaseURL:            srv.URL,
		Repo:               "org/api",
		TokenEnv:           "ITERION_TEST_FORGE_TOKEN",
	})
	if err == nil {
		t.Fatal("a refused page must surface: the board holds part of the window and the operator was told nothing")
	}
	// The board is re-opened cold, so the count in the message is checked
	// against what a dispatcher starting now would read, not against the
	// command's own bookkeeping.
	board, berr := native.NewStore(filepath.Join(storeDir, "dispatcher"))
	if berr != nil {
		t.Fatalf("reopen board: %v", berr)
	}
	t.Cleanup(func() { _ = board.Close() })
	cards, lerr := board.List(native.ListFilter{})
	if lerr != nil {
		t.Fatalf("list board: %v", lerr)
	}
	t.Logf("OBSERVED err=%v stdout=%q cards=%d", err, out.String(), len(cards))
	if len(cards) != landed {
		t.Fatalf("the board holds %d cards, want %d of the %d entries served — the premise is a refusal that DID write, and a pull request the board does not mirror",
			len(cards), landed, served)
	}
	// The premise of carrying them in the error: stdout says nothing about
	// this pass. If the counters were ever printed here too, one of the two
	// would have to go.
	if strings.Contains(out.String(), "Synced") {
		t.Errorf("stdout = %q already reports the counters on a refused import — the error need not carry them twice", out.String())
	}
	if !strings.Contains(err.Error(), "mirrored "+strconv.Itoa(landed)+" issues") {
		t.Errorf("error = %v, want the %d cards that landed named: the counters below the import are never printed on this path", err, landed)
	}
	if !strings.Contains(err.Error(), "HTTP 502") {
		t.Errorf("error = %v, want the forge's own refusal as the cause", err)
	}
}

// The same class, one pass later: `--project` runs AFTER the issue pass has
// written its cards, so a refused project pass hides exactly as much. The two
// error paths that can follow a written board share one sentence
// (importedSoFar) rather than one of them carrying it.
func TestRunIssueImport_ARefusedProjectPassStillReportsTheIssuesThatLanded(t *testing.T) {
	const landed = 2
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/{owner}/{name}/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if page, _ := strconv.Atoi(r.URL.Query().Get("page")); page > 1 {
			_, _ = io.WriteString(w, `[]`) // the window ends here: the issue pass SUCCEEDS
			return
		}
		_, _ = io.WriteString(w, `[
  {"number":1,"title":"one","state":"open","html_url":"http://gh/1"},
  {"number":2,"title":"two","state":"open","html_url":"http://gh/2"}
]`)
	})
	// The project pass goes through GraphQL (<base>/api/graphql on a GitHub
	// Enterprise-shaped host); this instance refuses it.
	mux.HandleFunc("/api/graphql", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "the project API is down", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	storeDir := t.TempDir()
	t.Setenv("ITERION_TEST_FORGE_TOKEN", "tok")
	var out bytes.Buffer
	err := RunIssueImport(&Printer{W: &out, Format: OutputHuman}, IssueImportOptions{
		IssueCommonOptions: IssueCommonOptions{StoreDir: storeDir},
		Forge:              "github",
		BaseURL:            srv.URL,
		Repo:               "org/api",
		TokenEnv:           "ITERION_TEST_FORGE_TOKEN",
		Project:            "org/7",
	})
	if err == nil {
		t.Fatal("a refused project pass must surface")
	}
	board, berr := native.NewStore(filepath.Join(storeDir, "dispatcher"))
	if berr != nil {
		t.Fatalf("reopen board: %v", berr)
	}
	t.Cleanup(func() { _ = board.Close() })
	cards, lerr := board.List(native.ListFilter{})
	if lerr != nil {
		t.Fatalf("list board: %v", lerr)
	}
	t.Logf("OBSERVED err=%v stdout=%q cards=%d", err, out.String(), len(cards))
	if len(cards) != landed {
		t.Fatalf("the board holds %d cards, want %d — the premise is an issue pass that COMPLETED before the project pass failed", len(cards), landed)
	}
	if !strings.Contains(err.Error(), "mirrored "+strconv.Itoa(landed)+" issues") {
		t.Errorf("error = %v, want the %d issues the pass before it mirrored: the counters are never printed on this path either", err, landed)
	}
}
