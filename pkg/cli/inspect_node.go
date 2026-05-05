package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// nodeReport is the structured payload returned by `iterion inspect
// --node` / `--exec`. The shape is intentionally aligned with the
// editor's NodeDetailPanel so the same vocabulary
// (trace / tools / artifacts / interactions / events / log) is shared
// across surfaces. Optional sections use omitempty so the JSON output
// stays tight when the caller asked for one bucket.
type nodeReport struct {
	RunID       string             `json:"run_id"`
	NodeID      string             `json:"node_id"`
	BranchID    string             `json:"branch_id"`
	Iteration   int                `json:"iteration"`
	ExecutionID string             `json:"execution_id"`
	Kind        string             `json:"kind,omitempty"`
	Status      runview.ExecStatus `json:"status"`
	StartedAt   *time.Time         `json:"started_at,omitempty"`
	FinishedAt  *time.Time         `json:"finished_at,omitempty"`
	Duration    string             `json:"duration,omitempty"`
	FirstSeq    int64              `json:"first_seq"`
	LastSeq     int64              `json:"last_seq"`
	Error       string             `json:"error,omitempty"`

	Tokens  int     `json:"tokens,omitempty"`
	CostUSD float64 `json:"cost_usd,omitempty"`

	Trace        []llmStep            `json:"trace,omitempty"`
	Tools        []toolCallReport     `json:"tools,omitempty"`
	Artifacts    []nodeArtifact       `json:"artifacts,omitempty"`
	Interactions []interactionSummary `json:"interactions,omitempty"`
	Events       []*store.Event       `json:"events,omitempty"`
	Log          *nodeLogSlice        `json:"log,omitempty"`
}

// llmStep mirrors NodeDetailPanel.tsx's `useLLMSteps`: one entry per
// LLM turn within the execution.
type llmStep struct {
	Seq          int64         `json:"seq"`
	SystemPrompt string        `json:"system_prompt,omitempty"`
	UserMessage  string        `json:"user_message,omitempty"`
	Response     string        `json:"response,omitempty"`
	Model        string        `json:"model,omitempty"`
	InputTokens  int           `json:"input_tokens,omitempty"`
	OutputTokens int           `json:"output_tokens,omitempty"`
	FinishReason string        `json:"finish_reason,omitempty"`
	ToolCalls    []toolCallRef `json:"tool_calls,omitempty"`
	Pending      bool          `json:"pending,omitempty"`
}

type toolCallRef struct {
	ToolName string `json:"tool_name"`
	Input    string `json:"input,omitempty"`
}

type toolCallReport struct {
	Seq        int64                  `json:"seq"`
	ToolName   string                 `json:"tool_name"`
	IsError    bool                   `json:"is_error"`
	DurationMs int                    `json:"duration_ms,omitempty"`
	Input      string                 `json:"input,omitempty"`
	Output     string                 `json:"output,omitempty"`
	Error      string                 `json:"error,omitempty"`
	Raw        map[string]interface{} `json:"raw,omitempty"`
}

// nodeArtifact is one persisted artifact version. Body is included
// only when the caller explicitly asked for the artifacts section
// (or --full) so the default JSON stays small.
type nodeArtifact struct {
	Version   int                    `json:"version"`
	WrittenAt time.Time              `json:"written_at"`
	Data      map[string]interface{} `json:"data,omitempty"`
}

type interactionSummary struct {
	ID          string                 `json:"id"`
	RequestedAt time.Time              `json:"requested_at"`
	AnsweredAt  *time.Time             `json:"answered_at,omitempty"`
	Questions   map[string]interface{} `json:"questions,omitempty"`
	Answers     map[string]interface{} `json:"answers,omitempty"`
}

// nodeLogSlice is a best-effort timestamp-windowed slice of run.log.
// The free-form log format does not carry node IDs, so the slice is
// derived from the execution's [StartedAt, FinishedAt] range and
// flagged as best-effort. Multi-branch concurrent runs may interleave
// lines from sibling executions in this window — consumers should
// treat this as a hint rather than ground truth.
type nodeLogSlice = runview.LogSlice

// listNodeExecutions enumerates the ExecutionState rows of a run.
func listNodeExecutions(s store.RunStore, runID string, p *Printer) error {
	snap, err := runview.BuildSnapshot(s, runID)
	if err != nil {
		return fmt.Errorf("cannot build snapshot: %w", err)
	}

	if p.Format == OutputJSON {
		p.JSON(map[string]interface{}{
			"run_id":     runID,
			"executions": snap.Executions,
		})
		return nil
	}

	p.Header("Executions: " + runID)
	if len(snap.Executions) == 0 {
		p.Line("  (no node executions yet)")
		return nil
	}
	rows := make([][]string, 0, len(snap.Executions))
	for _, e := range snap.Executions {
		duration := "—"
		if e.StartedAt != nil && e.FinishedAt != nil {
			duration = FormatDuration(e.FinishedAt.Sub(*e.StartedAt))
		}
		rows = append(rows, []string{
			e.ExecutionID,
			e.IRNodeID,
			e.BranchID,
			strconv.Itoa(e.LoopIteration),
			e.Kind,
			string(e.Status),
			duration,
			fmt.Sprintf("%d-%d", e.FirstSeq, e.LastSeq),
		})
	}
	p.Table([]string{"EXEC", "NODE", "BRANCH", "ITER", "KIND", "STATUS", "DURATION", "SEQS"}, rows)
	return nil
}

func runInspectNode(s store.RunStore, storeDir string, opts InspectOptions, p *Printer) error {
	snap, err := runview.BuildSnapshot(s, opts.RunID)
	if err != nil {
		return fmt.Errorf("cannot build snapshot: %w", err)
	}
	exec, err := resolveExecution(snap, opts)
	if err != nil {
		return err
	}

	report, err := buildNodeReport(s, storeDir, opts, exec)
	if err != nil {
		return err
	}

	if p.Format == OutputJSON {
		p.JSON(report)
		return nil
	}
	renderNodeReport(report, p)
	return nil
}

// resolveExecution picks one ExecutionState from a snapshot using the
// caller's selection. Resolution order:
//
//  1. ExecutionID wins outright (exact match required).
//  2. Otherwise filter by Node + optional Branch.
//  3. If Iteration was set, pick that exact iteration.
//  4. Otherwise: single match wins; same-branch matches collapse to
//     latest (mirrors editor's auto-follow); cross-branch matches
//     error with candidate exec IDs.
func resolveExecution(snap *runview.RunSnapshot, opts InspectOptions) (*runview.ExecutionState, error) {
	if opts.ExecutionID != "" {
		for i := range snap.Executions {
			if snap.Executions[i].ExecutionID == opts.ExecutionID {
				return &snap.Executions[i], nil
			}
		}
		return nil, fmt.Errorf("execution %q not found%s", opts.ExecutionID, suggestExecs(snap, ""))
	}
	if opts.Node == "" {
		return nil, fmt.Errorf("--node or --exec is required")
	}

	var candidates []*runview.ExecutionState
	for i := range snap.Executions {
		e := &snap.Executions[i]
		if e.IRNodeID != opts.Node {
			continue
		}
		if opts.Branch != "" && e.BranchID != opts.Branch {
			continue
		}
		candidates = append(candidates, e)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no execution found for node %q%s", opts.Node, suggestExecs(snap, ""))
	}

	if opts.Iteration != nil && *opts.Iteration != IterationLatest {
		for _, c := range candidates {
			if c.LoopIteration == *opts.Iteration {
				return c, nil
			}
		}
		return nil, fmt.Errorf("no execution for node %q at iteration %d%s",
			opts.Node, *opts.Iteration, suggestExecs(snap, opts.Node))
	}

	if len(candidates) == 1 {
		return candidates[0], nil
	}

	branch := candidates[0].BranchID
	sameBranch := true
	for _, c := range candidates[1:] {
		if c.BranchID != branch {
			sameBranch = false
			break
		}
	}
	if sameBranch || (opts.Iteration != nil && *opts.Iteration == IterationLatest) {
		latest := candidates[0]
		for _, c := range candidates[1:] {
			if c.LoopIteration > latest.LoopIteration {
				latest = c
			}
		}
		return latest, nil
	}

	ids := make([]string, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.ExecutionID)
	}
	sort.Strings(ids)
	return nil, fmt.Errorf("node %q is ambiguous; specify --branch or --exec (candidates: %s)",
		opts.Node, strings.Join(ids, ", "))
}

func suggestExecs(snap *runview.RunSnapshot, nodeFilter string) string {
	var ids []string
	for _, e := range snap.Executions {
		if nodeFilter != "" && e.IRNodeID != nodeFilter {
			continue
		}
		ids = append(ids, e.ExecutionID)
	}
	if len(ids) == 0 {
		return ""
	}
	sort.Strings(ids)
	return " (available: " + strings.Join(ids, ", ") + ")"
}

func buildNodeReport(
	s store.RunStore,
	storeDir string,
	opts InspectOptions,
	exec *runview.ExecutionState,
) (*nodeReport, error) {
	report := &nodeReport{
		RunID:       opts.RunID,
		NodeID:      exec.IRNodeID,
		BranchID:    exec.BranchID,
		Iteration:   exec.LoopIteration,
		ExecutionID: exec.ExecutionID,
		Kind:        exec.Kind,
		Status:      exec.Status,
		StartedAt:   exec.StartedAt,
		FinishedAt:  exec.FinishedAt,
		FirstSeq:    exec.FirstSeq,
		LastSeq:     exec.LastSeq,
		Error:       exec.Error,
	}
	if exec.StartedAt != nil && exec.FinishedAt != nil {
		report.Duration = FormatDuration(exec.FinishedAt.Sub(*exec.StartedAt))
	}

	section := opts.Section
	if section == "" {
		section = SectionAll
	}
	want := func(s InspectSection) bool {
		return section == SectionAll || section == s
	}

	// Per-execution event slice underpins trace / tools / interactions /
	// log windows AND the tokens/cost totals; load it once unless the
	// caller asked only for the meta-only summary bucket.
	var matching []*store.Event
	if section != SectionSummary {
		var err error
		matching, err = loadExecEvents(s, opts.RunID, exec)
		if err != nil {
			return nil, fmt.Errorf("load events: %w", err)
		}
		report.Tokens, report.CostUSD = sumTokensCost(matching)
	}

	if section == SectionSummary {
		return report, nil
	}

	if want(SectionTrace) {
		report.Trace = buildLLMTrace(matching)
	}
	if want(SectionTools) {
		report.Tools = buildToolCalls(matching)
	}
	if want(SectionArtifacts) || opts.Full {
		var err error
		// Bodies are only loaded when the user explicitly drilled into
		// the artifacts section (or asked for --full); the default
		// section=all path returns the index only.
		report.Artifacts, err = buildArtifactList(s, opts.RunID, exec.IRNodeID, matching,
			section == SectionArtifacts || opts.Full)
		if err != nil {
			return nil, fmt.Errorf("load artifacts: %w", err)
		}
	}
	if want(SectionInteractions) {
		var err error
		report.Interactions, err = buildInteractionList(s, opts.RunID, exec.IRNodeID, matching)
		if err != nil {
			return nil, fmt.Errorf("load interactions: %w", err)
		}
	}
	if want(SectionEvents) {
		report.Events = matching
	}
	if want(SectionLog) {
		report.Log = buildLogSlice(storeDir, opts.RunID, exec, opts.LogTail)
	}

	return report, nil
}

// loadExecEvents streams events.jsonl through the [exec.FirstSeq,
// exec.LastSeq] window the snapshot reducer already computed and
// returns the events for this exec. Iteration matching uses a single
// counter for the target (branch, node) pair — events for other
// nodes/branches are skipped without touching state. Mirrors the
// per-(branch,node) counting in runview.SnapshotBuilder.
func loadExecEvents(s store.RunStore, runID string, exec *runview.ExecutionState) ([]*store.Event, error) {
	out := make([]*store.Event, 0, 32)
	iter := -1
	err := s.ScanEvents(runID, func(e *store.Event) bool {
		if e == nil {
			return true
		}
		if e.Seq > exec.LastSeq {
			return false
		}
		if e.NodeID != exec.IRNodeID {
			return true
		}
		branch := e.BranchID
		if branch == "" {
			branch = runview.MainBranch
		}
		if branch != exec.BranchID {
			return true
		}
		if e.Type == store.EventNodeStarted {
			iter++
		}
		if iter == exec.LoopIteration && e.Seq >= exec.FirstSeq {
			out = append(out, e)
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// buildLLMTrace is the Go port of NodeDetailPanel.useLLMSteps: walks
// llm_prompt → llm_request → llm_step_finished events accumulating
// one llmStep per turn. lastModel survives across steps so a model-
// only llm_request between turns still attributes the model to the
// next turn (matches the editor reducer).
func buildLLMTrace(events []*store.Event) []llmStep {
	steps := make([]llmStep, 0, 4)
	var current *llmStep
	var lastModel string

	flush := func() {
		if current != nil {
			steps = append(steps, *current)
			current = nil
		}
	}

	for _, e := range events {
		if e.Data == nil {
			continue
		}
		switch e.Type {
		case store.EventLLMPrompt:
			flush()
			current = &llmStep{
				Seq:          e.Seq,
				SystemPrompt: stringField(e.Data, "system_prompt"),
				UserMessage:  stringField(e.Data, "user_message"),
				Model:        lastModel,
				Pending:      true,
			}
		case store.EventLLMRequest:
			model := stringField(e.Data, "model")
			if model != "" {
				lastModel = model
			}
			if current != nil {
				if model != "" {
					current.Model = model
				}
			} else {
				current = &llmStep{Seq: e.Seq, Model: lastModel, Pending: true}
			}
		case store.EventLLMStepFinished:
			if current == nil {
				current = &llmStep{Seq: e.Seq}
			}
			if v := stringField(e.Data, "response_text"); v != "" {
				current.Response = v
			}
			if v := extractInt(e.Data, "input_tokens"); v != 0 {
				current.InputTokens = v
			}
			if v := extractInt(e.Data, "output_tokens"); v != 0 {
				current.OutputTokens = v
			}
			if v := stringField(e.Data, "finish_reason"); v != "" {
				current.FinishReason = v
			}
			if calls, ok := e.Data["tool_call_details"].([]interface{}); ok {
				for _, c := range calls {
					if m, ok := c.(map[string]interface{}); ok {
						current.ToolCalls = append(current.ToolCalls, toolCallRef{
							ToolName: stringField(m, "tool_name"),
							Input:    stringField(m, "input"),
						})
					}
				}
			}
			current.Pending = false
			flush()
		}
	}
	flush()
	return steps
}

func buildToolCalls(events []*store.Event) []toolCallReport {
	out := make([]toolCallReport, 0, 4)
	for _, e := range events {
		if e.Type != store.EventToolCalled && e.Type != store.EventToolError {
			continue
		}
		data := e.Data
		if data == nil {
			data = map[string]interface{}{}
		}
		name := stringField(data, "tool_name")
		if name == "" {
			name = stringField(data, "tool")
		}
		if name == "" {
			name = "unknown"
		}
		entry := toolCallReport{
			Seq:        e.Seq,
			ToolName:   name,
			IsError:    e.Type == store.EventToolError,
			DurationMs: extractInt(data, "duration_ms"),
			Input:      coerceString(data["input"]),
			Output:     coerceString(data["output"]),
			Raw:        data,
		}
		if entry.IsError {
			entry.Error = stringField(data, "error")
			if entry.Error == "" {
				entry.Error = stringField(data, "message")
			}
		}
		out = append(out, entry)
	}
	return out
}

// buildArtifactList enumerates only artifact versions written by the
// selected execution. Persisted artifact files are keyed by run/node/version,
// so directory enumeration alone cannot distinguish loop iterations or
// sibling branches of the same node. The per-execution event slice is the
// source of truth for scope; bodies are loaded only when includeBodies is true.
func buildArtifactList(s store.RunStore, runID, nodeID string, events []*store.Event, includeBodies bool) ([]nodeArtifact, error) {
	out := make([]nodeArtifact, 0, 2)
	seen := make(map[int]bool)
	for _, e := range events {
		if e == nil || e.Type != store.EventArtifactWritten || e.Data == nil {
			continue
		}
		version := extractInt(e.Data, "version")
		if seen[version] {
			continue
		}
		seen[version] = true

		entry := nodeArtifact{Version: version, WrittenAt: e.Timestamp}
		if includeBodies {
			a, err := s.LoadArtifact(runID, nodeID, version)
			if err != nil {
				continue
			}
			entry.WrittenAt = a.WrittenAt
			entry.Data = a.Data
		}
		out = append(out, entry)
	}
	return out, nil
}

// buildInteractionList collects interactions whose IDs appear in the
// per-execution event slice. Returns nil when no human_input_requested
// or human_answers_recorded event referenced an interaction in the
// window — that's the truthful answer ("no node-scoped interactions"),
// not a signal to scan the whole run.
func buildInteractionList(s store.RunStore, runID, nodeID string, events []*store.Event) ([]interactionSummary, error) {
	seen := make(map[string]bool)
	for _, e := range events {
		if e.Type != store.EventHumanInputRequested && e.Type != store.EventHumanAnswersRecorded {
			continue
		}
		if e.Data == nil {
			continue
		}
		if id := stringField(e.Data, "interaction_id"); id != "" {
			seen[id] = true
		}
	}
	if len(seen) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]interactionSummary, 0, len(keys))
	for _, id := range keys {
		inter, err := s.LoadInteraction(runID, id)
		if err != nil {
			continue
		}
		if inter.NodeID != "" && inter.NodeID != nodeID {
			continue
		}
		out = append(out, interactionSummary{
			ID:          inter.ID,
			RequestedAt: inter.RequestedAt,
			AnsweredAt:  inter.AnsweredAt,
			Questions:   inter.Questions,
			Answers:     inter.Answers,
		})
	}
	return out, nil
}

// buildLogSlice extracts the timestamp-windowed slice of run.log
// matching the execution. The log format (HH:MM:SS emoji msg) carries
// no node ID so the result is best-effort. When tail > 0 the matched
// region is kept in a rolling tail buffer instead of accumulating the
// whole window in memory and then trimming.
func buildLogSlice(storeDir, runID string, exec *runview.ExecutionState, tail int) *nodeLogSlice {
	return runview.BuildLogSlice(storeDir, runID, exec, tail)
}

func renderNodeReport(r *nodeReport, p *Printer) {
	p.Header(fmt.Sprintf("Node: %s [%s]", r.NodeID, r.ExecutionID))
	p.KV("Run", r.RunID)
	p.KV("Branch", r.BranchID)
	p.KV("Iteration", strconv.Itoa(r.Iteration))
	if r.Kind != "" {
		p.KV("Kind", r.Kind)
	}
	p.KV("Status", StatusIcon(string(r.Status))+" "+string(r.Status))
	if r.StartedAt != nil {
		p.KV("Started", FormatTime(*r.StartedAt))
	}
	if r.FinishedAt != nil {
		p.KV("Finished", FormatTime(*r.FinishedAt))
	}
	if r.Duration != "" {
		p.KV("Duration", r.Duration)
	}
	p.KV("Seqs", fmt.Sprintf("%d-%d", r.FirstSeq, r.LastSeq))
	if r.Tokens > 0 {
		p.KV("Tokens", strconv.Itoa(r.Tokens))
	}
	if r.CostUSD > 0 {
		p.KV("Cost", fmt.Sprintf("$%.4f", r.CostUSD))
	}
	if r.Error != "" {
		p.KV("Error", truncate(r.Error, 400))
	}

	if len(r.Trace) > 0 {
		p.Blank()
		p.Header(fmt.Sprintf("Trace (%d step%s)", len(r.Trace), pluralS(len(r.Trace))))
		for i, st := range r.Trace {
			summary := st.FinishReason
			if summary == "" {
				summary = "ok"
			}
			if st.Pending {
				summary = "pending"
			}
			p.Line("  step %d  seq=%d  model=%s  in=%d out=%d  finish=%s",
				i+1, st.Seq, dashIfEmpty(st.Model), st.InputTokens, st.OutputTokens, summary)
			if st.SystemPrompt != "" {
				p.Line("    system: %s", truncate(oneLine(st.SystemPrompt), 200))
			}
			if st.UserMessage != "" {
				p.Line("    user:   %s", truncate(oneLine(st.UserMessage), 200))
			}
			if st.Response != "" {
				p.Line("    reply:  %s", truncate(oneLine(st.Response), 200))
			}
			if len(st.ToolCalls) > 0 {
				names := make([]string, 0, len(st.ToolCalls))
				for _, c := range st.ToolCalls {
					names = append(names, c.ToolName)
				}
				p.Line("    tools:  %s", strings.Join(names, ", "))
			}
		}
	}

	if len(r.Tools) > 0 {
		p.Blank()
		p.Header(fmt.Sprintf("Tool calls (%d)", len(r.Tools)))
		rows := make([][]string, 0, len(r.Tools))
		for _, t := range r.Tools {
			status := "ok"
			if t.IsError {
				status = "error"
			}
			rows = append(rows, []string{
				strconv.FormatInt(t.Seq, 10),
				t.ToolName,
				status,
				formatMs(t.DurationMs),
				truncate(oneLine(t.Input), 80),
			})
		}
		p.Table([]string{"SEQ", "TOOL", "STATUS", "DURATION", "INPUT"}, rows)
	}

	if len(r.Artifacts) > 0 {
		p.Blank()
		p.Header(fmt.Sprintf("Artifacts (%d)", len(r.Artifacts)))
		rows := make([][]string, 0, len(r.Artifacts))
		for _, a := range r.Artifacts {
			summary := ""
			if a.Data != nil {
				if s, ok := a.Data["summary"].(string); ok {
					summary = truncate(oneLine(s), 80)
				}
			}
			rows = append(rows, []string{
				"v" + strconv.Itoa(a.Version),
				FormatTime(a.WrittenAt),
				summary,
			})
		}
		p.Table([]string{"VERSION", "WRITTEN", "SUMMARY"}, rows)
	}

	if len(r.Interactions) > 0 {
		p.Blank()
		p.Header(fmt.Sprintf("Interactions (%d)", len(r.Interactions)))
		for _, i := range r.Interactions {
			p.KV("ID", i.ID)
			p.KV("Requested", FormatTime(i.RequestedAt))
			if i.AnsweredAt != nil {
				p.KV("Answered", FormatTime(*i.AnsweredAt))
			}
			if len(i.Questions) > 0 {
				keys := make([]string, 0, len(i.Questions))
				for k := range i.Questions {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				p.Line("  Questions: %s", strings.Join(keys, ", "))
			}
			p.Blank()
		}
	}

	if len(r.Events) > 0 {
		p.Blank()
		p.Header(fmt.Sprintf("Events (%d)", len(r.Events)))
		rows := make([][]string, 0, len(r.Events))
		for _, e := range r.Events {
			rows = append(rows, []string{
				strconv.FormatInt(e.Seq, 10),
				string(e.Type),
				e.NodeID,
				FormatTime(e.Timestamp),
			})
		}
		p.Table([]string{"SEQ", "TYPE", "NODE", "TIMESTAMP"}, rows)
	}

	if r.Log != nil {
		p.Blank()
		p.Header("Log slice (best-effort)")
		p.KV("Window", fmt.Sprintf("%s → %s", FormatTime(r.Log.StartTime), FormatTime(r.Log.EndTime)))
		p.KV("Bytes", fmt.Sprintf("%d-%d", r.Log.StartByte, r.Log.EndByte))
		if r.Log.Truncated {
			p.KV("Truncated", "yes")
		}
		for _, n := range r.Log.Notes {
			p.KV("Note", n)
		}
		if r.Log.Body != "" {
			p.Blank()
			for _, ln := range strings.Split(strings.TrimRight(r.Log.Body, "\n"), "\n") {
				p.Line("  %s", ln)
			}
		}
	}
}

func sumTokensCost(events []*store.Event) (int, float64) {
	var tokens int
	var cost float64
	for _, e := range events {
		if e.Type != store.EventNodeFinished || e.Data == nil {
			continue
		}
		tokens += extractTokens(e.Data)
		cost += extractCost(e.Data)
	}
	return tokens, cost
}

func stringField(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func coerceString(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return safeJSON(v)
}

func safeJSON(v interface{}) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(s)
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func formatMs(ms int) string {
	if ms <= 0 {
		return "—"
	}
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}
