package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/benchmark/asymptote"
	"github.com/SocialGouv/iterion/pkg/benchmark/discovery"
	"github.com/SocialGouv/iterion/pkg/store"
)

// BenchAsymptoteOptions configures the `iterion bench asymptote` command.
//
// The asymptote thesis (see docs/why-iterion.md) holds that running the
// same workflow across N independent sessions produces a quality curve
// that climbs and stabilises — that stabilisation is the asymptote, and
// it measures the (model + recipe)'s reliability ceiling for the task.
//
// Primary use: feed N runs of the same workflow via --runs and observe
// the per-iteration aggregate + cross-session distribution.
//
// Secondary use: feed an alternative recipe variant via --variant-runs
// to quantify a lift (typically a multi-family alternation variant for
// security-critical or complex tasks). Multi-family alternation is *not*
// the default thesis — it is the optional refinement.
type BenchAsymptoteOptions struct {
	StoreDir          string
	Runs              []string // canonical asymptote subjects (same workflow, N independent sessions)
	VariantRuns       []string // optional: alternative recipe variant for comparison
	Label             string   // primary group label (default: "asymptote")
	VariantLabel      string   // variant group label (default: "variant")
	JudgeNode         string   // IR node ID of the judge whose verdicts we score
	JudgeField        string   // output field name (default "approved")
	LoopName          string   // optional: pin to one loop (default: first observed)
	ApprovalThreshold float64  // default 0.5
	Output            string   // markdown path; "-" or "" → stdout
	Title             string
	IncludePerRun     bool
}

// RunBenchAsymptote loads the requested runs, parses each with the asymptote
// pipeline, aggregates per group, renders, and writes the report.
func RunBenchAsymptote(opts BenchAsymptoteOptions, p *Printer) error {
	if opts.JudgeNode == "" {
		return fmt.Errorf("--judge-node is required (the IR node ID of the judge whose verdicts will be scored)")
	}
	if len(opts.Runs) == 0 && len(opts.VariantRuns) == 0 {
		return fmt.Errorf("at least one of --runs or --variant-runs must be provided")
	}
	if opts.JudgeField == "" {
		opts.JudgeField = asymptote.DefaultJudgeField
	}
	if opts.ApprovalThreshold == 0 {
		opts.ApprovalThreshold = asymptote.DefaultApprovalThreshold
	}
	if opts.Label == "" {
		opts.Label = "asymptote"
	}
	if opts.VariantLabel == "" {
		opts.VariantLabel = "variant"
	}

	cwd, _ := os.Getwd()
	storeDir := store.ResolveStoreDir(cwd, opts.StoreDir)

	s, err := store.New(storeDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}

	parseOpts := asymptote.ParseOptions{
		JudgeNodeID:       opts.JudgeNode,
		JudgeField:        opts.JudgeField,
		LoopName:          opts.LoopName,
		ApprovalThreshold: opts.ApprovalThreshold,
	}

	ctx := context.Background()

	primarySeries, err := parseRuns(ctx, s, opts.Runs, parseOpts)
	if err != nil {
		return err
	}
	variantSeries, err := parseRuns(ctx, s, opts.VariantRuns, parseOpts)
	if err != nil {
		return err
	}

	cmp := asymptote.Compare(
		asymptote.AggregateGroup(opts.Label, primarySeries),
		asymptote.AggregateGroup(opts.VariantLabel, variantSeries),
	)

	if p.Format == OutputJSON {
		p.JSON(cmp)
		return nil
	}

	md := asymptote.RenderMarkdown(cmp, asymptote.RenderOptions{
		Title:             opts.Title,
		GeneratedAt:       time.Now().UTC(),
		ApprovalThreshold: opts.ApprovalThreshold,
		IncludePerRun:     opts.IncludePerRun,
	})

	if opts.Output == "" || opts.Output == "-" {
		_, _ = fmt.Fprint(p.W, md)
		return nil
	}

	if err := os.WriteFile(opts.Output, []byte(md), 0o644); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	p.Line("Report written to %s", opts.Output)
	return nil
}

// BenchDiscoveryOptions configures the `iterion bench discovery` command.
//
// Discovery measures what a run spent finding its way around the tree —
// the reads, searches and listings that precede a change — against what
// it spent on the change. See pkg/benchmark/discovery for what the event
// stream can and cannot attribute.
type BenchDiscoveryOptions struct {
	StoreDir string
	Runs     []string // explicit subjects
	Last     int      // or: the N most recently created runs in the store
	Output   string   // markdown path; "-" or "" → stdout
	Title    string
	TopN     int // rows in the per-node and per-verb tables (0 → 15)
}

// RunBenchDiscovery profiles the requested runs and renders the report.
func RunBenchDiscovery(opts BenchDiscoveryOptions, p *Printer) error {
	if len(opts.Runs) == 0 && opts.Last <= 0 {
		return fmt.Errorf("pass --runs <id,...> or --last <n>: discovery has no default corpus")
	}

	cwd, _ := os.Getwd()
	storeDir := store.ResolveStoreDir(cwd, opts.StoreDir)
	s, err := store.New(storeDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	ctx := context.Background()

	ids := opts.Runs
	var unreadable []string
	// undated is a different population from unreadable and is reported
	// apart: these runs are anywhere in the store, and because their
	// metadata would not load, nobody knows their date — so nobody can
	// say whether they belonged in the requested window. Calling them
	// "requested and excluded" asserts a fact the selector never had.
	var undated []string
	if len(ids) == 0 {
		ids, undated, err = recentRunIDs(ctx, s, opts.Last)
		if err != nil {
			return err
		}
	}

	profiles := make([]*discovery.RunProfile, 0, len(ids))
	for _, id := range ids {
		prof, err := discovery.ParseRun(ctx, s, id)
		if err != nil {
			// A run whose store entry cannot be read is reported, never
			// silently dropped: a corpus that shrank without saying so
			// would move every ratio below it.
			unreadable = append(unreadable, id)
			continue
		}
		profiles = append(profiles, prof)
	}
	if len(profiles) == 0 {
		return fmt.Errorf("no readable run among the %d requested (store %s)", len(ids), storeDir)
	}

	corpus := discovery.Aggregate(profiles)

	if p.Format == OutputJSON {
		p.JSON(struct {
			Corpus     discovery.Corpus        `json:"corpus"`
			Runs       []*discovery.RunProfile `json:"runs"`
			Unreadable []string                `json:"unreadable,omitempty"`
			Undated    []string                `json:"undated,omitempty"`
		}{corpus, profiles, unreadable, undated})
		return nil
	}

	md := discovery.RenderMarkdown(corpus, profiles, discovery.RenderOptions{
		Title:       opts.Title,
		GeneratedAt: time.Now().UTC(),
		StoreLabel:  storeDir,
		TopN:        opts.TopN,
	})
	if len(unreadable) > 0 {
		md += fmt.Sprintf("\n%d requested run(s) could not be read and are excluded: %s\n",
			len(unreadable), strings.Join(unreadable, ", "))
	}
	if len(undated) > 0 {
		md += fmt.Sprintf("\n%d run(s) in the store carry no readable metadata, so the selector "+
			"could not date them and cannot say whether they belonged in this window: %s\n",
			len(undated), strings.Join(undated, ", "))
	}

	if opts.Output == "" || opts.Output == "-" {
		_, _ = fmt.Fprint(p.W, md)
		return nil
	}
	if err := os.WriteFile(opts.Output, []byte(md), 0o644); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	p.Line("Report written to %s", opts.Output)
	return nil
}

// recentRunIDs returns the n most recently CREATED runs, plus the ids it
// could not read. The order comes from each run's own created_at, never
// from the order ListRuns happens to return: a directory listing is not
// a chronology.
func recentRunIDs(ctx context.Context, s store.RunStore, n int) (ids, skipped []string, err error) {
	all, err := s.ListRuns(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list runs: %w", err)
	}
	type dated struct {
		id string
		at time.Time
	}
	rows := make([]dated, 0, len(all))
	for _, id := range all {
		run, err := s.LoadRun(ctx, id)
		if err != nil {
			skipped = append(skipped, id)
			continue
		}
		rows = append(rows, dated{id, run.CreatedAt})
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].at.Equal(rows[j].at) {
			return rows[i].at.After(rows[j].at)
		}
		return rows[i].id > rows[j].id // stable tie-break, never the listing order
	})
	if len(rows) > n {
		rows = rows[:n]
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.id)
	}
	return out, skipped, nil
}

// SplitRunIDs parses a comma-separated CLI flag value into a slice of run IDs,
// trimming whitespace and skipping empty entries.
func SplitRunIDs(csv string) []string {
	if csv == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseRuns(ctx context.Context, s store.RunStore, ids []string, opts asymptote.ParseOptions) ([]asymptote.RunSeries, error) {
	out := make([]asymptote.RunSeries, 0, len(ids))
	for _, id := range ids {
		rs, err := asymptote.ParseRun(ctx, s, id, opts)
		if err != nil {
			return nil, fmt.Errorf("parse run %s: %w", id, err)
		}
		out = append(out, *rs)
	}
	return out, nil
}
