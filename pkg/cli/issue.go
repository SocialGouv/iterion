package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/server"
	"github.com/SocialGouv/iterion/pkg/store"
)

// IssueCommonOptions are flags shared by every `iterion issue` subcommand.
type IssueCommonOptions struct {
	StoreDir string
}

// openNativeStore resolves <store-dir>/dispatcher and opens the native
// store there. The directory and board.json are created on first call.
func openNativeStore(opts IssueCommonOptions) (*native.Store, string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, "", err
	}
	storeDir := store.ResolveStoreDir(cwd, opts.StoreDir)
	root := filepath.Join(storeDir, "dispatcher")
	s, err := native.NewStore(root)
	if err != nil {
		return nil, "", err
	}
	return s, root, nil
}

// IssueCreateOptions captures the flags for `iterion issue create`.
type IssueCreateOptions struct {
	IssueCommonOptions
	Title    string
	Body     string
	State    string
	Labels   []string
	Priority int
	Assignee string
	Blockers []string
	Fields   []string // custom-field key=value pairs (Issue.Fields)
	Bot      string   // typed workflow override consumed by the dispatcher
	BotArgs  []string // typed dispatcher var overrides as key=value (Issue.BotArgs)
}

// RunIssueCreate creates a new issue in the native tracker.
func RunIssueCreate(p *Printer, opts IssueCreateOptions) error {
	if opts.Title == "" {
		return errors.New("issue create: --title is required")
	}
	s, _, err := openNativeStore(opts.IssueCommonOptions)
	if err != nil {
		return err
	}
	fields, err := parseFieldPairs(opts.Fields)
	if err != nil {
		return err
	}
	botArgs, err := parseBotArgs(opts.BotArgs)
	if err != nil {
		return err
	}
	iss := native.Issue{
		Title:    opts.Title,
		Body:     opts.Body,
		State:    opts.State,
		Labels:   opts.Labels,
		Priority: opts.Priority,
		Assignee: opts.Assignee,
		Blockers: opts.Blockers,
		Fields:   fields,
		Bot:      opts.Bot,
		BotArgs:  botArgs,
	}
	got, err := s.Create(iss)
	if err != nil {
		return err
	}
	if p.Format == OutputJSON {
		p.JSON(got)
		return nil
	}
	p.Line("Created %s", shortID(got.ID))
	p.KV("State", got.State)
	if got.Title != "" {
		p.KV("Title", got.Title)
	}
	return nil
}

// IssueListOptions captures the flags for `iterion issue list`.
type IssueListOptions struct {
	IssueCommonOptions
	States    []string
	Labels    []string
	Assignee  string
	OnlyClaim bool
	OnlyFree  bool
}

// RunIssueList prints issues from the native tracker.
func RunIssueList(p *Printer, opts IssueListOptions) error {
	s, _, err := openNativeStore(opts.IssueCommonOptions)
	if err != nil {
		return err
	}
	f := native.ListFilter{
		States:   opts.States,
		Labels:   opts.Labels,
		Assignee: opts.Assignee,
	}
	switch {
	case opts.OnlyClaim:
		t := true
		f.Claimed = &t
	case opts.OnlyFree:
		t := false
		f.Claimed = &t
	}
	issues, err := s.List(f)
	if err != nil {
		return err
	}
	if p.Format == OutputJSON {
		p.JSON(issues)
		return nil
	}
	rows := make([][]string, 0, len(issues))
	for _, iss := range issues {
		rows = append(rows, []string{
			shortID(iss.ID),
			iss.State,
			strconv.Itoa(iss.Priority),
			truncate(iss.Title, 50),
			strings.Join(iss.Labels, ","),
			iss.Assignee,
		})
	}
	p.Table([]string{"ID", "STATE", "PRIO", "TITLE", "LABELS", "ASSIGNEE"}, rows)
	return nil
}

// IssueRefOptions identifies a single issue by ID or prefix.
type IssueRefOptions struct {
	IssueCommonOptions
	IDOrPrefix string
}

// RunIssueShow prints a single issue.
func RunIssueShow(p *Printer, opts IssueRefOptions) error {
	s, _, err := openNativeStore(opts.IssueCommonOptions)
	if err != nil {
		return err
	}
	id, err := s.Resolve(opts.IDOrPrefix)
	if err != nil {
		return err
	}
	iss, err := s.Get(id)
	if err != nil {
		return err
	}
	if p.Format == OutputJSON {
		p.JSON(iss)
		return nil
	}
	p.Header(iss.Title)
	p.KV("ID", iss.ID)
	p.KV("State", iss.State)
	p.KV("Priority", strconv.Itoa(iss.Priority))
	if iss.Assignee != "" {
		p.KV("Assignee", iss.Assignee)
	}
	if len(iss.Labels) > 0 {
		p.KV("Labels", strings.Join(iss.Labels, ", "))
	}
	if len(iss.Blockers) > 0 {
		p.KV("Blockers", strings.Join(iss.Blockers, ", "))
	}
	if iss.Claim != "" {
		p.KV("Claim", iss.Claim)
	}
	if len(iss.Fields) > 0 {
		data, _ := json.MarshalIndent(iss.Fields, "  ", "  ")
		p.Line("  Fields:")
		p.Line("    %s", string(data))
	}
	if iss.Body != "" {
		p.Blank()
		p.Line("%s", iss.Body)
	}
	return nil
}

// IssueMoveOptions moves an issue to a new state.
type IssueMoveOptions struct {
	IssueCommonOptions
	IDOrPrefix string
	To         string
}

// RunIssueMove transitions an issue.
func RunIssueMove(p *Printer, opts IssueMoveOptions) error {
	if opts.To == "" {
		return errors.New("issue move: --to is required")
	}
	s, _, err := openNativeStore(opts.IssueCommonOptions)
	if err != nil {
		return err
	}
	id, err := s.Resolve(opts.IDOrPrefix)
	if err != nil {
		return err
	}
	iss, err := s.SetState(id, opts.To)
	if err != nil {
		return err
	}
	if p.Format == OutputJSON {
		p.JSON(iss)
		return nil
	}
	p.Line("Moved %s → %s", shortID(iss.ID), iss.State)
	return nil
}

// IssueUpdateOptions captures partial-update fields. Nil pointer means "unchanged".
type IssueUpdateOptions struct {
	IssueCommonOptions
	IDOrPrefix string
	Title      *string
	Body       *string
	Labels     *[]string
	Priority   *int
	Assignee   *string
	Blockers   *[]string
	Fields     []string // key=value (set or replace)
	ClearField []string
	// ClearLastRun drops the issue's last_run pointer (Store.SetLastRun with
	// empty strings — the clear its own contract documents). It is the
	// operator's way back to a FRESH launch: while the pointer names a
	// resumable run, the dispatcher resumes that run instead of minting a new
	// one (resolveRunID → resumableRunID), so a ticket whose run died in a way
	// resuming cannot fix had no exit but hand-editing the issue JSON.
	// The run history (Issue.Runs) is kept — the runs still happened.
	ClearLastRun bool
}

// RunIssueUpdate applies the patch.
func RunIssueUpdate(p *Printer, opts IssueUpdateOptions) error {
	s, _, err := openNativeStore(opts.IssueCommonOptions)
	if err != nil {
		return err
	}
	id, err := s.Resolve(opts.IDOrPrefix)
	if err != nil {
		return err
	}
	fields, err := parseFieldPairs(opts.Fields)
	if err != nil {
		return err
	}
	for _, k := range opts.ClearField {
		if fields == nil {
			fields = map[string]any{}
		}
		fields[k] = nil
	}
	patch := native.Patch{
		Title:    opts.Title,
		Body:     opts.Body,
		Labels:   opts.Labels,
		Priority: opts.Priority,
		Assignee: opts.Assignee,
		Blockers: opts.Blockers,
		Fields:   fields,
	}
	iss, err := s.Update(id, patch)
	if err != nil {
		return err
	}
	if opts.ClearLastRun {
		if err := s.SetLastRun(id, "", ""); err != nil {
			return err
		}
		if iss, err = s.Get(id); err != nil {
			return err
		}
	}
	if p.Format == OutputJSON {
		p.JSON(iss)
		return nil
	}
	p.Line("Updated %s", shortID(iss.ID))
	if opts.ClearLastRun {
		p.Line("Cleared the last-run pointer — the next dispatch starts a fresh run instead of resuming.")
	}
	return nil
}

// RunIssueClose moves the issue to the first terminal state on the board.
func RunIssueClose(p *Printer, opts IssueRefOptions) error {
	s, _, err := openNativeStore(opts.IssueCommonOptions)
	if err != nil {
		return err
	}
	id, err := s.Resolve(opts.IDOrPrefix)
	if err != nil {
		return err
	}
	board := s.Board()
	terminal := ""
	for _, st := range board.States {
		if st.Terminal {
			terminal = st.Name
			break
		}
	}
	if terminal == "" {
		return errors.New("issue close: board has no terminal state — declare one or use `issue move`")
	}
	if _, err := s.SetState(id, terminal); err != nil {
		return err
	}
	// Closing is the operator ACKNOWLEDGING the ticket, so drop any
	// dispatcher give-up stamp on it. Moving a ticket expires the stamp on
	// its own, but closing one the dispatcher already filed into this very
	// state changes nothing — and would leave the card sitting in the
	// pipeline board's needs-attention lane after the operator closed it.
	// Best-effort, like the pipeline board's Close: SetState has already
	// committed, so failing here would report a close that DID happen as a
	// failure. A surviving stamp costs a card in the wrong lane, not
	// correctness — say so and carry on.
	if err := s.SetGaveUp(id, nil); err != nil {
		p.Line("warning: could not clear the dispatcher give-up stamp on %s: %v", shortID(id), err)
	}
	iss, err := s.Get(id)
	if err != nil {
		return err
	}
	if p.Format == OutputJSON {
		p.JSON(iss)
		return nil
	}
	p.Line("Closed %s → %s", shortID(iss.ID), iss.State)
	return nil
}

// IssueImportOptions captures the flags for `iterion issue import`: mirror a
// self-hosted forge repo's issues into the native board.
type IssueImportOptions struct {
	IssueCommonOptions
	Forge    string // github | forgejo | gitlab
	Repo     string // owner/name
	TokenEnv string // NAME of the env var holding the forge token (never the value)
	BaseURL  string // forge API base; empty = provider default
	Since    string // RFC3339; empty = full re-sync
	// MinAuthorRole is the trust threshold (gitlab vocabulary; "" →
	// developer) above which an issue author's fresh card is stamped
	// triage:auto instead of parked needs:approval.
	MinAuthorRole string
}

// RunIssueImport imports a forge repo's issues into the native board, reusing
// the store-agnostic sync core via server.ImportForgeIssues. The token is read
// only from the NAMED env var (secrets discipline — never a flag value). The
// import is idempotent: re-running upserts existing cards instead of
// duplicating them.
func RunIssueImport(p *Printer, opts IssueImportOptions) error {
	provider := forge.Provider(strings.TrimSpace(opts.Forge))
	if !provider.Valid() {
		return fmt.Errorf("issue import: --forge must be one of github|forgejo|gitlab, got %q", opts.Forge)
	}
	if strings.TrimSpace(opts.Repo) == "" {
		return errors.New("issue import: --repo is required (owner/name)")
	}
	if strings.TrimSpace(opts.TokenEnv) == "" {
		return errors.New("issue import: --token-env is required (name of the env var holding the forge token)")
	}
	token := os.Getenv(opts.TokenEnv)
	if token == "" {
		return fmt.Errorf("issue import: env var %q is empty or unset", opts.TokenEnv)
	}
	var since time.Time
	if s := strings.TrimSpace(opts.Since); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return fmt.Errorf("issue import: --since must be RFC3339, got %q: %w", opts.Since, err)
		}
		since = t
	}
	board, _, err := openNativeStore(opts.IssueCommonOptions)
	if err != nil {
		return err
	}
	created, updated, err := server.ImportForgeIssues(
		context.Background(), provider, strings.TrimSpace(opts.BaseURL), token, opts.Repo, board, since,
		strings.TrimSpace(opts.MinAuthorRole))
	if err != nil {
		return fmt.Errorf("issue import: %w", err)
	}
	if p.Format == OutputJSON {
		p.JSON(struct {
			Created int `json:"created"`
			Updated int `json:"updated"`
		}{Created: created, Updated: updated})
		return nil
	}
	p.Line("Imported %s from %s", opts.Repo, provider)
	p.KV("Created", strconv.Itoa(created))
	p.KV("Updated", strconv.Itoa(updated))
	return nil
}

// RunIssueBoardShow prints the current board.json.
func RunIssueBoardShow(p *Printer, opts IssueCommonOptions) error {
	s, root, err := openNativeStore(opts)
	if err != nil {
		return err
	}
	b := s.Board()
	if p.Format == OutputJSON {
		p.JSON(b)
		return nil
	}
	p.Header("Board")
	p.KV("Location", filepath.Join(root, "board.json"))
	p.Blank()
	p.Line("States:")
	for _, st := range b.States {
		tag := ""
		if st.Eligible {
			tag += " (eligible)"
		}
		if st.Terminal {
			tag += " (terminal)"
		}
		p.Line("  - %s%s", st.Name, tag)
	}
	if len(b.Fields) > 0 {
		p.Blank()
		p.Line("Fields:")
		for _, f := range b.Fields {
			req := ""
			if f.Required {
				req = " (required)"
			}
			p.Line("  - %s : %s%s", f.Name, f.Type, req)
			if f.Type == native.FieldEnum {
				p.Line("      values: %s", strings.Join(f.EnumValues, ", "))
			}
		}
	}
	return nil
}

// IssueBoardInitOptions configures `issue board init`.
type IssueBoardInitOptions struct {
	IssueCommonOptions
	From  string // optional path to a board.json
	Force bool
}

// RunIssueBoardInit replaces the board configuration.
func RunIssueBoardInit(p *Printer, opts IssueBoardInitOptions) error {
	s, root, err := openNativeStore(opts.IssueCommonOptions)
	if err != nil {
		return err
	}
	var b *native.Board
	if opts.From != "" {
		data, err := os.ReadFile(opts.From)
		if err != nil {
			return fmt.Errorf("read %s: %w", opts.From, err)
		}
		b = &native.Board{}
		if err := json.Unmarshal(data, b); err != nil {
			return fmt.Errorf("parse %s: %w", opts.From, err)
		}
	} else {
		b = native.DefaultBoard()
	}
	if err := s.SetBoard(b); err != nil {
		return err
	}
	if p.Format == OutputJSON {
		p.JSON(b)
		return nil
	}
	p.Line("Board initialized at %s", filepath.Join(root, "board.json"))
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// parseFieldPairs converts ["k=v", "k2=42"] into typed values. Numbers,
// bools, and bare strings are auto-detected. Quoted values are kept
// literal.
func parseFieldPairs(pairs []string) (map[string]any, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	return parseKVPairs[any](pairs, kvOpts[any]{
		errFmt:        "--field expects key=value, got %q",
		trimKey:       true,
		trimVal:       true,
		requireRawKey: true,
		conv:          func(v string) any { return inferTyped(v) },
	})
}

// parseBotArgs parses repeatable --bot-arg key=value flags into the
// typed BotArgs map the dispatcher merges over rendered config vars
// (pkg/dispatcher/loop.go). Unlike parseFieldPairs (custom fields,
// map[string]any with type inference), dispatcher vars are always
// strings — matching the studio Launch form wire format — so values
// are kept verbatim (no inferTyped, no comma-splitting: glob lists like
// doc_globs=README.md,docs/**/*.md must survive intact, which is why
// the flag is registered as StringArray, not StringSlice).
func parseBotArgs(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	return parseKVPairs[string](pairs, kvOpts[string]{
		errFmt:        "--bot-arg expects key=value, got %q",
		trimKey:       true,
		trimVal:       true,
		requireRawKey: true,
	})
}

func inferTyped(s string) any {
	switch s {
	case "true":
		return true
	case "false":
		return false
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}

func shortID(id string) string {
	s := strings.TrimPrefix(id, "native:")
	if len(s) > 8 {
		s = s[:8]
	}
	return s
}
