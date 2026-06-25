package quality

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Snapshot is one assessed run, persisted as a single JSON file in the
// target's append-only history directory. It bundles the price metrics,
// the panel aggregate, an evidence digest, and (when a prior snapshot
// existed) a deterministic comparison against it.
type Snapshot struct {
	SchemaVersion  int       `json:"schema_version"`
	Kind           string    `json:"kind"` // "bot" | "feature"
	Name           string    `json:"name"`
	Persona        string    `json:"persona,omitempty"`
	RunID          string    `json:"run_id"`
	At             time.Time `json:"at"`
	BotVersion     string    `json:"bot_version,omitempty"`
	IterionSHA     string    `json:"iterion_sha,omitempty"`
	Task           string    `json:"task,omitempty"`
	Metrics        Metrics   `json:"metrics"`
	Aggregate      Aggregate `json:"aggregate"`
	EvidenceDigest string    `json:"evidence_digest,omitempty"` // diffstat / counts / report excerpt
	PrevRunID      string    `json:"prev_run_id,omitempty"`
	Comparison     *Delta    `json:"comparison,omitempty"` // vs the previous snapshot, computed deterministically
}

// Overall returns the aggregated overall score (0 if absent).
func (s *Snapshot) Overall() float64 { return s.Aggregate.MeanScores[DimOverall] }

// Value returns the aggregated value-for-money score (0 if absent).
func (s *Snapshot) Value() float64 { return s.Aggregate.MeanScores[DimValueForMoney] }

// Delta is the deterministic comparison of a current snapshot against the
// previous one. It is descriptive; the regression decision (with a
// tolerance) is IsRegression so the same Delta can drive both reporting
// and an opt-in gate.
type Delta struct {
	PrevRunID    string                `json:"prev_run_id"`
	OverallDelta float64               `json:"overall_delta"` // cur.overall - prev.overall
	ValueDelta   float64               `json:"value_delta"`   // cur.value_for_money - prev
	PerDimension map[Dimension]float64 `json:"per_dimension"` // cur - prev, per dimension
	Verdict      Relative              `json:"verdict"`       // headline (overall) better/same/worse
}

const (
	// DefaultSameBand is the |Δ| within which an overall change is called
	// "same" rather than better/worse — absorbs LLM-judge noise.
	DefaultSameBand = 0.05
	// DefaultRegressTolerance is how far overall (or value) must drop below
	// the previous snapshot before the opt-in gate fails the test. Wider
	// than the same-band so only clear regressions trip it.
	DefaultRegressTolerance = 0.08
)

// Compare returns nil when prev is nil (no baseline yet); otherwise a
// per-dimension delta with a headline verdict on the overall score.
func Compare(prev, cur *Snapshot) *Delta {
	if prev == nil || cur == nil {
		return nil
	}
	d := &Delta{
		PrevRunID:    prev.RunID,
		OverallDelta: cur.Overall() - prev.Overall(),
		ValueDelta:   cur.Value() - prev.Value(),
		PerDimension: make(map[Dimension]float64, len(Dimensions)),
	}
	for _, dim := range Dimensions {
		d.PerDimension[dim] = cur.Aggregate.MeanScores[dim] - prev.Aggregate.MeanScores[dim]
	}
	switch {
	case d.OverallDelta > DefaultSameBand:
		d.Verdict = RelBetter
	case d.OverallDelta < -DefaultSameBand:
		d.Verdict = RelWorse
	default:
		d.Verdict = RelSame
	}
	return d
}

// IsRegression reports whether the overall OR value-for-money score
// dropped more than tol below the previous snapshot, with human-readable
// reasons. A nil Delta (no baseline) is never a regression.
func (d *Delta) IsRegression(tol float64) (bool, []string) {
	if d == nil {
		return false, nil
	}
	if tol <= 0 {
		tol = DefaultRegressTolerance
	}
	var reasons []string
	if d.OverallDelta < -tol {
		reasons = append(reasons, fmt.Sprintf("overall dropped %.3f (vs %s)", d.OverallDelta, d.PrevRunID))
	}
	if d.ValueDelta < -tol {
		reasons = append(reasons, fmt.Sprintf("value_for_money dropped %.3f (vs %s)", d.ValueDelta, d.PrevRunID))
	}
	return len(reasons) > 0, reasons
}

// SnapshotStore is the committed, append-only history root. In the e2e
// suite Root is e2e/testdata/live/quality; each target gets a subdir of
// per-run JSON files named "<UTC-ts>__<runid>.json" so lexical order is
// chronological.
type SnapshotStore struct {
	Root string
}

// NewSnapshotStore roots a store at dir.
func NewSnapshotStore(dir string) *SnapshotStore { return &SnapshotStore{Root: dir} }

// Dir returns the (sanitised) per-target history directory.
func (s *SnapshotStore) Dir(name string) string {
	return filepath.Join(s.Root, sanitizeSegment(name))
}

// fileName derives the deterministic per-run file name from a snapshot.
func fileName(snap *Snapshot) string {
	ts := snap.At.UTC().Format("20060102T150405Z")
	return fmt.Sprintf("%s__%s.json", ts, sanitizeSegment(snap.RunID))
}

// Write persists snap as a new history file and returns its path. It is
// deterministic given the snapshot (filename derives from At+RunID), so
// re-writing the same snapshot is idempotent.
func (s *SnapshotStore) Write(snap *Snapshot) (string, error) {
	if snap == nil {
		return "", fmt.Errorf("quality: nil snapshot")
	}
	if snap.SchemaVersion == 0 {
		snap.SchemaVersion = SchemaVersion
	}
	dir := s.Dir(snap.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("quality: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, fileName(snap))
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return "", fmt.Errorf("quality: marshal snapshot: %w", err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return "", fmt.Errorf("quality: write %s: %w", path, err)
	}
	return path, nil
}

// List returns the target's history file paths in chronological (lexical)
// order. A missing directory yields an empty slice, not an error.
func (s *SnapshotStore) List(name string) ([]string, error) {
	dir := s.Dir(name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("quality: read %s: %w", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out, nil
}

// Load reads a single snapshot file.
func (s *SnapshotStore) Load(path string) (*Snapshot, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("quality: read %s: %w", path, err)
	}
	var snap Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, fmt.Errorf("quality: parse %s: %w", path, err)
	}
	return &snap, nil
}

// Last returns the most recent prior snapshot for a target (the history
// tail), or (nil,false,nil) when none exists.
func (s *SnapshotStore) Last(name string) (*Snapshot, bool, error) {
	paths, err := s.List(name)
	if err != nil {
		return nil, false, err
	}
	if len(paths) == 0 {
		return nil, false, nil
	}
	snap, err := s.Load(paths[len(paths)-1])
	if err != nil {
		return nil, false, err
	}
	return snap, true, nil
}

// sanitizeSegment makes a name safe as a single path segment (no
// separators, no traversal).
func sanitizeSegment(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, string(filepath.Separator), "-")
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, "\\", "-")
	name = strings.ReplaceAll(name, "..", "-")
	name = strings.Trim(name, ".-")
	if name == "" {
		return "unnamed"
	}
	return name
}
