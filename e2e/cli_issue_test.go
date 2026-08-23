package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// `iterion issue` is the operator's hands-on surface onto the native
// kanban the dispatcher polls. The store and the HTTP surface are
// unit-covered; the COMMAND wrappers were not — and they are what an
// operator actually types. What is observable: the card that lands on
// disk (title/labels/priority/state), the state transitions, the patch
// semantics (nil = unchanged), `close` picking the board's first
// terminal state, and the append-only audit trail in events.jsonl that
// the dispatcher and the studio both read.
//
// Every assertion re-opens the board through a FRESH native.Store, so
// what is asserted is the persisted truth, never the CLI's in-memory
// index.
//
// Mutation check: stop persisting on create and the reload finds
// nothing; drop the state write in `move` and the transition assertion
// fails; make `close` pick any non-terminal state and the terminal
// assertion fails; let `update` clobber unset fields and the
// "unchanged" assertions fail; stop appending events and the audit
// trail assertion fails.

// issueBoardDir is the dispatcher root `iterion issue` writes under a
// given --store-dir.
func issueBoardDir(storeDir string) string {
	return filepath.Join(storeDir, "dispatcher")
}

// reopenBoard reads the board back through a fresh store — the state a
// dispatcher process starting cold would see.
func reopenBoard(t *testing.T, storeDir string) *native.Store {
	t.Helper()
	s, err := native.NewStore(issueBoardDir(storeDir))
	if err != nil {
		t.Fatalf("reopen native store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// issueEvents reads the persisted audit trail.
func issueEvents(t *testing.T, storeDir string) []native.Event {
	t.Helper()
	f, err := os.Open(filepath.Join(issueBoardDir(storeDir), "events.jsonl"))
	if err != nil {
		t.Fatalf("open events.jsonl: %v", err)
	}
	defer func() { _ = f.Close() }()

	var out []native.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev native.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("decode event %q: %v", line, err)
		}
		out = append(out, ev)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan events.jsonl: %v", err)
	}
	return out
}

// createIssueViaCLI runs `issue create` in JSON mode and returns the
// created card as the command reported it.
func createIssueViaCLI(t *testing.T, opts cli.IssueCreateOptions) native.Issue {
	t.Helper()
	var buf bytes.Buffer
	p := &cli.Printer{W: &buf, Format: cli.OutputJSON}
	if err := cli.RunIssueCreate(p, opts); err != nil {
		t.Fatalf("issue create: %v", err)
	}
	var iss native.Issue
	if err := json.Unmarshal(buf.Bytes(), &iss); err != nil {
		t.Fatalf("decode created issue from %q: %v", buf.String(), err)
	}
	if iss.ID == "" {
		t.Fatalf("issue create returned no id: %s", buf.String())
	}
	return iss
}

func TestIssueCLILifecycleCreateMoveUpdateClose(t *testing.T) {
	storeDir := t.TempDir()
	common := cli.IssueCommonOptions{StoreDir: storeDir}

	card := createIssueViaCLI(t, cli.IssueCreateOptions{
		IssueCommonOptions: common,
		Title:              "wire the coverage gate",
		Body:               "the matrix must be greppable",
		State:              native.StateReady,
		Labels:             []string{"kind:test", "area:e2e"},
		Priority:           3,
		Assignee:           "endy",
		Bot:                "bots/feature-dev/main.bot",
		BotArgs:            []string{"scope=e2e"},
	})

	// A second card, so the list filters have something to exclude and
	// a blanket "return everything" implementation cannot pass.
	other := createIssueViaCLI(t, cli.IssueCreateOptions{
		IssueCommonOptions: common,
		Title:              "unrelated backlog item",
		State:              native.StateBacklog,
		Labels:             []string{"kind:chore"},
	})

	// --- create landed on disk, with every declared attribute ---
	{
		got, err := reopenBoard(t, storeDir).Get(card.ID)
		if err != nil {
			t.Fatalf("reload created issue: %v", err)
		}
		if got.Title != "wire the coverage gate" {
			t.Errorf("title = %q, want %q", got.Title, "wire the coverage gate")
		}
		if got.Body != "the matrix must be greppable" {
			t.Errorf("body = %q, want the created body", got.Body)
		}
		if got.State != native.StateReady {
			t.Errorf("state = %q, want %q", got.State, native.StateReady)
		}
		if got.Priority != 3 {
			t.Errorf("priority = %d, want 3", got.Priority)
		}
		if got.Assignee != "endy" {
			t.Errorf("assignee = %q, want endy", got.Assignee)
		}
		if !hasLabel(got.Labels, "kind:test") || !hasLabel(got.Labels, "area:e2e") {
			t.Errorf("labels = %v, want both created labels", got.Labels)
		}
		// The dispatcher override an operator pins on the card is what
		// makes `issue create --bot` more than a note-to-self.
		if got.Bot != "bots/feature-dev/main.bot" {
			t.Errorf("bot = %q, want the pinned workflow override", got.Bot)
		}
		if got.BotArgs["scope"] != "e2e" {
			t.Errorf("bot_args = %#v, want scope=e2e", got.BotArgs)
		}
	}

	// --- list filters on state, not "everything on the board" ---
	{
		var buf bytes.Buffer
		p := &cli.Printer{W: &buf, Format: cli.OutputJSON}
		if err := cli.RunIssueList(p, cli.IssueListOptions{
			IssueCommonOptions: common,
			States:             []string{native.StateReady},
		}); err != nil {
			t.Fatalf("issue list: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, card.ID) {
			t.Errorf("list --state ready omitted the ready card %s:\n%s", card.ID, out)
		}
		if strings.Contains(out, other.ID) {
			t.Errorf("list --state ready leaked the backlog card %s:\n%s", other.ID, out)
		}
	}

	// --- show resolves a short prefix to the full card ---
	{
		// A partial id, the way an operator copies one off `issue list`.
		// Keep enough of the uuid to stay unambiguous against the second
		// card: the ids share the "native:" scheme prefix, so a truncation
		// that keeps only a hex digit or two collides by chance.
		prefix := idPrefix(t, card.ID, 8)
		var buf bytes.Buffer
		p := &cli.Printer{W: &buf, Format: cli.OutputJSON}
		if err := cli.RunIssueShow(p, cli.IssueRefOptions{
			IssueCommonOptions: common,
			IDOrPrefix:         prefix,
		}); err != nil {
			t.Fatalf("issue show by prefix: %v", err)
		}
		var shown native.Issue
		if err := json.Unmarshal(buf.Bytes(), &shown); err != nil {
			t.Fatalf("decode shown issue: %v", err)
		}
		if shown.ID != card.ID {
			t.Errorf("show %s resolved to %s, want %s", prefix, shown.ID, card.ID)
		}
	}

	// --- move transitions the persisted state ---
	{
		var buf bytes.Buffer
		p := &cli.Printer{W: &buf, Format: cli.OutputJSON}
		if err := cli.RunIssueMove(p, cli.IssueMoveOptions{
			IssueCommonOptions: common,
			IDOrPrefix:         card.ID,
			To:                 native.StateInProgress,
		}); err != nil {
			t.Fatalf("issue move: %v", err)
		}
		got, err := reopenBoard(t, storeDir).Get(card.ID)
		if err != nil {
			t.Fatalf("reload moved issue: %v", err)
		}
		if got.State != native.StateInProgress {
			t.Errorf("after move, state = %q, want %q", got.State, native.StateInProgress)
		}
	}

	// --- update patches only what it was given ---
	{
		newTitle := "wire the coverage gate (deterministic)"
		newPriority := 1
		var buf bytes.Buffer
		p := &cli.Printer{W: &buf, Format: cli.OutputJSON}
		if err := cli.RunIssueUpdate(p, cli.IssueUpdateOptions{
			IssueCommonOptions: common,
			IDOrPrefix:         card.ID,
			Title:              &newTitle,
			Priority:           &newPriority,
		}); err != nil {
			t.Fatalf("issue update: %v", err)
		}
		got, err := reopenBoard(t, storeDir).Get(card.ID)
		if err != nil {
			t.Fatalf("reload updated issue: %v", err)
		}
		if got.Title != newTitle {
			t.Errorf("after update, title = %q, want %q", got.Title, newTitle)
		}
		if got.Priority != 1 {
			t.Errorf("after update, priority = %d, want 1", got.Priority)
		}
		// Untouched fields survive the patch — a full-overwrite update
		// would have blanked these.
		if got.Assignee != "endy" {
			t.Errorf("after update, assignee = %q, want the untouched endy", got.Assignee)
		}
		if !hasLabel(got.Labels, "kind:test") {
			t.Errorf("after update, labels = %v, want the untouched labels", got.Labels)
		}
		if got.State != native.StateInProgress {
			t.Errorf("after update, state = %q, want the untouched %q", got.State, native.StateInProgress)
		}
	}

	// --- close lands on the board's first TERMINAL state ---
	{
		var buf bytes.Buffer
		p := &cli.Printer{W: &buf, Format: cli.OutputJSON}
		if err := cli.RunIssueClose(p, cli.IssueRefOptions{
			IssueCommonOptions: common,
			IDOrPrefix:         card.ID,
		}); err != nil {
			t.Fatalf("issue close: %v", err)
		}
		s := reopenBoard(t, storeDir)
		got, err := s.Get(card.ID)
		if err != nil {
			t.Fatalf("reload closed issue: %v", err)
		}
		st := s.Board().StateByName(got.State)
		if st == nil || !st.Terminal {
			t.Fatalf("close left the card in %q, which is not a terminal board state", got.State)
		}
		if got.State != native.StateDone {
			t.Errorf("close landed on %q, want the first terminal state %q", got.State, native.StateDone)
		}
	}

	// --- the audit trail records the whole lifecycle, in order ---
	{
		events := issueEvents(t, storeDir)
		var seen []native.EventType
		lastSeq := int64(-1)
		for _, ev := range events {
			if ev.Seq <= lastSeq {
				t.Fatalf("events.jsonl seq not monotonic: %d after %d", ev.Seq, lastSeq)
			}
			lastSeq = ev.Seq
			if ev.IssueID == card.ID {
				seen = append(seen, ev.Type)
			}
		}
		want := []native.EventType{
			native.EvtIssueCreated,
			native.EvtIssueState, // move
			native.EvtIssueUpdated,
			native.EvtIssueState, // close
		}
		if !containsInOrder(seen, want) {
			t.Errorf("audit trail for %s = %v, want %v in order", card.ID, seen, want)
		}
	}
}

// idPrefix truncates an issue id to its scheme plus n identifying
// characters, and fails the test if that is not a STRICT prefix — a
// "prefix" equal to the full id would silently stop exercising
// Store.Resolve's prefix path.
func idPrefix(t *testing.T, id string, n int) string {
	t.Helper()
	scheme := strings.Index(id, ":") + 1
	if scheme <= 0 || len(id) <= scheme+n {
		t.Fatalf("issue id %q is too short to truncate to a %d-char prefix", id, n)
	}
	return id[:scheme+n]
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

// containsInOrder reports whether want appears as a subsequence of got.
func containsInOrder(got, want []native.EventType) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}

// A board whose states are all non-terminal has nothing for `close` to
// aim at. The command must say so rather than silently leaving the card
// where it was — an operator who typed `close` and saw success would
// otherwise carry a card that never left the board.
func TestIssueCloseRefusesABoardWithNoTerminalState(t *testing.T) {
	storeDir := t.TempDir()
	common := cli.IssueCommonOptions{StoreDir: storeDir}

	card := createIssueViaCLI(t, cli.IssueCreateOptions{
		IssueCommonOptions: common,
		Title:              "nowhere to close to",
		State:              native.StateReady,
	})

	// Rewrite the board with every terminal flag cleared.
	s := reopenBoard(t, storeDir)
	board := s.Board()
	for i := range board.States {
		board.States[i].Terminal = false
	}
	if err := s.SetBoard(board); err != nil {
		t.Fatalf("rewrite board: %v", err)
	}
	_ = s.Close()

	var buf bytes.Buffer
	p := &cli.Printer{W: &buf, Format: cli.OutputJSON}
	err := cli.RunIssueClose(p, cli.IssueRefOptions{
		IssueCommonOptions: common,
		IDOrPrefix:         card.ID,
	})
	if err == nil {
		t.Fatalf("issue close on a board with no terminal state succeeded, output:\n%s", buf.String())
	}
	if !strings.Contains(err.Error(), "terminal state") {
		t.Errorf("error = %v, want it to name the missing terminal state", err)
	}

	got, err := reopenBoard(t, storeDir).Get(card.ID)
	if err != nil {
		t.Fatalf("reload issue: %v", err)
	}
	if got.State != native.StateReady {
		t.Errorf("a refused close moved the card to %q; it must stay put", got.State)
	}
}

// `--clear-last-run` is the operator's way back to a FRESH launch. While the
// pointer names a resumable run the dispatcher resumes THAT run rather than
// minting a new one, so a ticket whose run died in a way resuming cannot fix
// had no exit but editing the issue JSON by hand (issue #494). The pointer
// clears; the run HISTORY does not — those runs still happened, and the
// operator still needs their consoles.
//
// Mutation check: keep the pointer and the "cleared" assertion fails; wipe
// Runs along with it and the history assertion fails.
func TestIssueCLIUpdateClearsLastRunPointerKeepingHistory(t *testing.T) {
	storeDir := t.TempDir()
	common := cli.IssueCommonOptions{StoreDir: storeDir}

	card := createIssueViaCLI(t, cli.IssueCreateOptions{
		IssueCommonOptions: common,
		Title:              "resumed forever",
		State:              native.StateBlocked,
	})

	s := reopenBoard(t, storeDir)
	if err := s.SetLastRun(card.ID, "run-dead", "/tmp/ws"); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}

	var buf bytes.Buffer
	p := &cli.Printer{W: &buf, Format: cli.OutputJSON}
	if err := cli.RunIssueUpdate(p, cli.IssueUpdateOptions{
		IssueCommonOptions: common,
		IDOrPrefix:         card.ID,
		ClearLastRun:       true,
	}); err != nil {
		t.Fatalf("issue update --clear-last-run: %v", err)
	}

	got, err := reopenBoard(t, storeDir).Get(card.ID)
	if err != nil {
		t.Fatalf("reload issue: %v", err)
	}
	if got.LastRunID != "" || got.LastWorkdir != "" {
		t.Errorf("last-run pointer survived: run=%q workdir=%q", got.LastRunID, got.LastWorkdir)
	}
	if len(got.Runs) != 1 || got.Runs[0].RunID != "run-dead" {
		t.Errorf("run history = %+v, want the dead run kept (its console is still the evidence)", got.Runs)
	}
	// The command reports the cleared card, not the pre-clear snapshot.
	var reported native.Issue
	if err := json.Unmarshal(buf.Bytes(), &reported); err != nil {
		t.Fatalf("decode reported issue from %q: %v", buf.String(), err)
	}
	if reported.LastRunID != "" {
		t.Errorf("reported last_run_id = %q, want empty — the output contradicted the write", reported.LastRunID)
	}
}
