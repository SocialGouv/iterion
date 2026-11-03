package liveledger

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeTB drives Track through a test-double that implements the narrow
// TB interface. Unlike testing.TB, the narrow interface has no
// unexported methods, so it can be implemented here — and the cleanup
// closure Track registers runs when finish() replays it, exactly as the
// testing framework would after a body completes.
type fakeTB struct {
	failed   bool
	skipped  bool
	cleanups []func()
	// logs keeps what the writer told the operator, so a test can hold the
	// writer to saying where a row went.
	logs []string
}

func (f *fakeTB) Cleanup(fn func()) { f.cleanups = append(f.cleanups, fn) }
func (f *fakeTB) Failed() bool      { return f.failed }
func (f *fakeTB) Skipped() bool     { return f.skipped }
func (f *fakeTB) Helper()           {}
func (f *fakeTB) Logf(format string, args ...any) {
	f.logs = append(f.logs, fmt.Sprintf(format, args...))
}
func (f *fakeTB) Name() string { return "fakeTB" }

// finish replays the cleanups the way the testing framework does after
// the test body returns.
func (f *fakeTB) finish() {
	for _, fn := range f.cleanups {
		fn()
	}
}

// TestTrack_CleanupWritesPassRowFromRealSubtest drives the REAL testing
// framework: the subtest registers a t.Cleanup through Track and the
// framework runs it when the subtest completes. This is the writer path
// the feature rests on — env in, row out.
func TestTrack_CleanupWritesPassRowFromRealSubtest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	t.Setenv(LedgerPathEnv, path)
	t.Setenv(EnvTarget, "test:live:probe")
	resetProcessWritten()

	t.Run("body passes", func(t *testing.T) {
		Track(t)
	})

	l, err := Load(path)
	if err != nil {
		t.Fatalf("ledger not written by the subtest's cleanup: %v", err)
	}
	row, ok := l.Get("test:live:probe")
	if !ok {
		t.Fatal("no row for the probe target — the cleanup did not write")
	}
	if row.Verdict != VerdictPass {
		t.Fatalf("passing subtest recorded %q, want pass", row.Verdict)
	}
	if row.Target != "test:live:probe" {
		t.Fatalf("row keyed %q, want the env-provided target", row.Target)
	}
}

// A body that failed must record fail. The verdict comes from the
// framework's Failed() at cleanup time, not from the row's previous
// state.
func TestTrack_CleanupWritesFailRowWhenBodyFailed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	t.Setenv(LedgerPathEnv, path)
	t.Setenv(EnvTarget, "test:live:probe")
	resetProcessWritten()

	fake := &fakeTB{}
	Track(fake)
	fake.failed = true
	fake.finish()

	l, err := Load(path)
	if err != nil {
		t.Fatalf("ledger not written: %v", err)
	}
	row, ok := l.Get("test:live:probe")
	if !ok {
		t.Fatal("no row written")
	}
	if row.Verdict != VerdictFail {
		t.Fatalf("failing body recorded %q, want fail", row.Verdict)
	}
}

// A skipped test never ran, so it claims neither pass nor fail — but it
// records a `skipped` row naming the prerequisite it lacked. Before this,
// a skip wrote nothing and the target stayed indistinguishable from one
// nobody has ever paid for: `never` meant both "unpaid" and "unrunnable
// here", and the matrix cited the second as the first.
func TestTrack_CleanupWritesSkippedRowNamingThePrerequisite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	t.Setenv(LedgerPathEnv, path)
	t.Setenv(EnvTarget, "test:live:probe")
	resetProcessWritten()

	fake := &fakeTB{}
	tr := Track(fake)
	tr.SetSkipReason("OPENAI_COMPATIBLE_API_KEY is not set")
	fake.skipped = true
	fake.finish()

	l, err := Load(path)
	if err != nil {
		t.Fatalf("skipped body wrote no ledger file: %v", err)
	}
	row, ok := l.Get("test:live:probe")
	if !ok {
		t.Fatal("no row for the skipped target — the skip left no trace at all")
	}
	if row.Verdict != VerdictSkipped {
		t.Fatalf("skipped body recorded %q, want skipped", row.Verdict)
	}
	if row.SkipReason != "OPENAI_COMPATIBLE_API_KEY is not set" {
		t.Fatalf("row skip_reason = %q, want the prerequisite the guard named", row.SkipReason)
	}
	if !row.LastRun.IsZero() || row.DurationSec != 0 || row.CostUSD != 0 {
		t.Fatalf("a skip claimed a run: %+v — nothing ran, so LastRun/duration/cost stay zero", row)
	}
	if row.SkippedAt.IsZero() {
		t.Fatal("the skip carries no date — a reader cannot tell a skip from last week from one from today")
	}
}

// A skip did not run, so it must NEVER erase what a run proved: the
// recorded verdict, its date, its cost and its provenance survive, and
// the skip lands beside them. The opposite — a `skipped` row replacing a
// known `fail` — would make the suite read healthier than it is.
func TestASkipNeverErasesARecordedVerdict(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	ran := time.Unix(1_700_000_000, 0).UTC()
	if err := RecordRow(path, Row{
		Target: "test:live:probe", Verdict: VerdictFail, LastRun: ran,
		DurationSec: 42, CostUSD: 0.01, Ref: "abc123abc123",
		ServedModel: "openai_compatible/gpt-oss-120b", FundedBy: "OPENAI_COMPATIBLE_API_KEY",
	}); err != nil {
		t.Fatalf("seed the recorded failure: %v", err)
	}

	t.Setenv(LedgerPathEnv, path)
	t.Setenv(EnvTarget, "test:live:probe")
	resetProcessWritten()
	RecordSkip(&fakeTB{}, "the gateway credential is absent from the environment")

	l, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	row, _ := l.Get("test:live:probe")
	if row.Verdict != VerdictFail {
		t.Fatalf("verdict is now %q — a skip overwrote a verdict a real run recorded", row.Verdict)
	}
	if !row.LastRun.Equal(ran) || row.DurationSec != 42 || row.CostUSD != 0.01 || row.Ref != "abc123abc123" {
		t.Fatalf("the recorded run lost data to a skip: %+v", row)
	}
	if row.ServedModel != "openai_compatible/gpt-oss-120b" || row.FundedBy != "OPENAI_COMPATIBLE_API_KEY" {
		t.Fatalf("the recorded provenance lost data to a skip: %+v", row)
	}
	if row.SkipReason != "the gateway credential is absent from the environment" || row.SkippedAt.IsZero() {
		t.Fatalf("the skip left no trace next to the verdict: %+v", row)
	}
}

// The row names what ACTUALLY served and which route funded it. Without
// these two columns a `pass` obtained on an overridden model reads exactly
// like a `pass` on the fixture's pinned model — the false receipt an
// overridden re-prove would otherwise mint.
func TestTheLedgerRowNamesTheModelThatServed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	t.Setenv(LedgerPathEnv, path)
	t.Setenv(EnvTarget, "test:live:probe")
	resetProcessWritten()

	fake := &fakeTB{}
	tr := Track(fake)
	tr.SetProvenance(FoldServedModels([]string{"openai_compatible/gpt-oss-120b"}), "OPENAI_COMPATIBLE_API_KEY")
	fake.finish()

	l, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := l.Get("test:live:probe")
	if !ok {
		t.Fatal("no row written")
	}
	if row.ServedModel != "openai_compatible/gpt-oss-120b" {
		t.Fatalf("row served_model = %q — the row reads as if the fixture's pinned model had answered", row.ServedModel)
	}
	if row.FundedBy != "OPENAI_COMPATIBLE_API_KEY" {
		t.Fatalf("row funded_by = %q, want the credential route that paid", row.FundedBy)
	}
}

// FoldServedModels is a SET keyed on a sort: a run whose steps resolved
// several specs reports the same column whichever order they finished in.
// A column reconstructed from that order would be a different receipt per
// run of the same suite.
func TestTheServedModelFoldIsOrderIndependent(t *testing.T) {
	a := FoldServedModels([]string{"openai/gpt-5.5", "anthropic/claude-sonnet-4-6", "openai/gpt-5.5"})
	b := FoldServedModels([]string{"anthropic/claude-sonnet-4-6", "openai/gpt-5.5"})
	if a != b {
		t.Fatalf("the fold depends on the order: %q vs %q", a, b)
	}
	if a != "anthropic/claude-sonnet-4-6,openai/gpt-5.5" {
		t.Fatalf("fold = %q, want the sorted distinct set", a)
	}
	if got := FoldServedModels(nil); got != "" {
		t.Fatalf("a tool-only run folded to %q, want the empty column — nothing served it", got)
	}
}

// An aggregate target merges its members in whatever order the runner
// chose: the folded row must be identical in BOTH orders, provenance and
// skip reasons included, and a member that skipped must not hide a member
// that passed.
func TestTheAggregateFoldIsIdenticalInBothOrders(t *testing.T) {
	passed := Row{Target: "t", Verdict: VerdictPass, LastRun: time.Unix(1_700_000_100, 0).UTC(),
		DurationSec: 7, CostUSD: 0.7, ServedModel: "openai/gpt-5.5", FundedBy: "A"}
	skipped := Row{Target: "t", Verdict: VerdictSkipped, SkippedAt: time.Unix(1_700_000_200, 0).UTC(),
		SkipReason: "no docker image", ServedModel: "anthropic/claude-sonnet-4-6", FundedBy: "B"}
	forward := mergeRows(passed, skipped)
	backward := mergeRows(skipped, passed)
	forward.Target, backward.Target = "t", "t"
	if forward != backward {
		t.Fatalf("the merge depends on the order:\n forward=%+v\nbackward=%+v", forward, backward)
	}
	if forward.Verdict != VerdictPass {
		t.Fatalf("merged verdict %q — a member that could not run must not erase a member that passed", forward.Verdict)
	}
	if forward.SkipReason != "no docker image" {
		t.Fatalf("merged skip_reason = %q — a half-skipped target must say which half", forward.SkipReason)
	}
	// A reason is PROSE and contains commas. Folding reasons on the list
	// separator would cut one sentence into fragments on the next merge,
	// and the ledger would carry a reason nobody wrote.
	prose := Row{Target: "t", Verdict: VerdictSkipped, SkipReason: "OPENAI_COMPATIBLE_API_KEY is not set, and this test does not guess"}
	other := Row{Target: "t", Verdict: VerdictSkipped, SkipReason: "no docker image"}
	folded := mergeRows(prose, other).SkipReason
	if !strings.Contains(folded, prose.SkipReason) {
		t.Fatalf("folded skip_reason = %q — the comma inside a reason split it into fragments", folded)
	}
	if mergeRows(other, prose).SkipReason != folded {
		t.Fatalf("the reason fold depends on the order: %q vs %q", mergeRows(other, prose).SkipReason, folded)
	}
	// Re-folding an already-folded column is idempotent: a third member
	// must not multiply the reasons.
	if again := mergeRows(mergeRows(prose, other), other).SkipReason; again != folded {
		t.Fatalf("re-folding changed the column: %q vs %q", again, folded)
	}
	if forward.ServedModel != "anthropic/claude-sonnet-4-6,openai/gpt-5.5" || forward.FundedBy != "A,B" {
		t.Fatalf("merged provenance %+v, want the sorted union of both members", forward)
	}
	if got := mergeRows(skipped, skipped).Verdict; got != VerdictSkipped {
		t.Fatalf("skipped+skipped = %q, want skipped", got)
	}
	never := Row{Target: "t", Verdict: VerdictNever}
	if got := mergeRows(never, skipped).Verdict; got != VerdictSkipped {
		t.Fatalf("never+skipped = %q, want skipped — an attempt that named its missing prerequisite beats no attempt at all", got)
	}
}

// One target ran several test functions in ONE process (the aggregate
// targets): the row must merge fail-sticky. fail-then-pass must report
// fail — last-write-wins would let a suite that failed report `pass`
// because its last member happened to pass — and duration/cost
// accumulate to the suite's totals.
func TestTrack_AggregateTargetMergesFailSticky(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	t.Setenv(LedgerPathEnv, path)
	t.Setenv(EnvTarget, "test:live:aggregate-probe")
	resetProcessWritten()

	start := time.Now().UTC()
	first := &fakeTB{}
	firstTr := Track(first)
	firstTr.SetStartedAt(start)
	firstTr.SetCost(0.25)
	first.failed = true
	first.finish()

	second := &fakeTB{}
	secondTr := Track(second)
	secondTr.SetStartedAt(start.Add(5 * time.Second))
	secondTr.SetCost(0.10)
	second.finish()

	l, err := Load(path)
	if err != nil {
		t.Fatalf("ledger file not written: %v", err)
	}
	row, ok := l.Get("test:live:aggregate-probe")
	if !ok {
		t.Fatal("no row written")
	}
	if row.Verdict != VerdictFail {
		t.Fatalf("fail-then-pass recorded %q — last-write-wins hid the suite's failure", row.Verdict)
	}
	if row.CostUSD < 0.34 || row.CostUSD > 0.36 {
		t.Fatalf("costs did not accumulate: %v, want ~0.25+0.10", row.CostUSD)
	}
}

// The merge must not depend on test order: pass-then-fail fails too
// (the same two-meanings bug, read from the other end).
func TestTrack_AggregateTargetMergesPassThenFail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	t.Setenv(LedgerPathEnv, path)
	t.Setenv(EnvTarget, "test:live:aggregate-probe")
	resetProcessWritten()

	first := &fakeTB{}
	firstTr := Track(first)
	firstTr.SetCost(0.10)
	first.finish()

	second := &fakeTB{}
	secondTr := Track(second)
	secondTr.SetCost(0.10)
	second.failed = true
	second.finish()

	l, err := Load(path)
	if err != nil {
		t.Fatalf("ledger not written: %v", err)
	}
	row, _ := l.Get("test:live:aggregate-probe")
	if row.Verdict != VerdictFail {
		t.Fatalf("pass-then-fail recorded %q, want fail", row.Verdict)
	}
	if row.CostUSD < 0.19 || row.CostUSD > 0.21 {
		t.Fatalf("costs did not accumulate: %v, want ~0.20", row.CostUSD)
	}
}

// Different targets on the same ledger file are independent rows — the
// merge registry is keyed per target, and a target run alone never sees
// another target's verdict or metrics.
func TestTrack_DifferentTargetsStayIndependent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	t.Setenv(LedgerPathEnv, path)
	resetProcessWritten()

	t.Setenv(EnvTarget, "test:live:probe-a")
	a := &fakeTB{}
	aTr := Track(a)
	aTr.SetCost(0.10)
	a.finish()

	t.Setenv(EnvTarget, "test:live:probe-b")
	b := &fakeTB{}
	bTr := Track(b)
	bTr.SetCost(0.20)
	b.failed = true
	b.finish()

	l, err := Load(path)
	if err != nil {
		t.Fatalf("ledger not written: %v", err)
	}
	rowA, okA := l.Get("test:live:probe-a")
	rowB, okB := l.Get("test:live:probe-b")
	if !okA || !okB {
		t.Fatalf("missing rows: a=%v b=%v", okA, okB)
	}
	if rowA.Verdict != VerdictPass || rowA.CostUSD < 0.09 || rowA.CostUSD > 0.11 {
		t.Fatalf("probe-a contaminated: %+v", rowA)
	}
	if rowB.Verdict != VerdictFail || rowB.CostUSD < 0.19 || rowB.CostUSD > 0.21 {
		t.Fatalf("probe-b contaminated: %+v", rowB)
	}
}

// OFFLINE mode (unit tests, offline probes) disables the writer: no
// cleanup registration, no file.
func TestTrack_OfflineDisablesWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	t.Setenv(LedgerPathEnv, path)
	t.Setenv(EnvOffline, "true")
	t.Setenv(EnvTarget, "test:live:probe")

	tr := Track(t)
	if !tr.disabled {
		t.Fatal("ITERION_LIVE_LEDGER_OFFLINE=true did not disable the tracker")
	}
}

// A missing target env leaves the tracker disabled and writes nothing —
// the Logf line is the only trace, so the operator can see why the row
// was not updated.
func TestTrack_MissingTargetEnvWritesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	t.Setenv(LedgerPathEnv, path)
	t.Setenv(EnvTarget, "")
	resetProcessWritten()

	fake := &fakeTB{}
	tr := Track(fake)
	if !tr.disabled {
		t.Fatal("missing target env did not disable the tracker")
	}
	fake.finish()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("disabled tracker wrote a file: %v", err)
	}
}

// mergeRows folds two rows of one target from one process; the exact
// arithmetic is what the aggregate rows rely on: verdicts fail-dominant,
// durations sum, and a never carries no verdict to contaminate the pair.
func TestMergeRows_Arithmetic(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0).UTC()
	t1 := time.Unix(1_700_000_100, 0).UTC()
	fail5 := Row{Target: "t", Verdict: VerdictFail, LastRun: t0, DurationSec: 5, CostUSD: 0.5}
	pass7 := Row{Target: "t", Verdict: VerdictPass, LastRun: t1, DurationSec: 7, CostUSD: 0.7}
	got := mergeRows(fail5, pass7)
	if got.Verdict != VerdictFail || got.DurationSec != 12 || got.CostUSD < 1.19 || got.CostUSD > 1.21 || !got.LastRun.Equal(t1) {
		t.Fatalf("fail5+pass7 = %+v, want fail, dur 12, cost ~1.2, lastRun t1", got)
	}
	// Order-independent verdict: pass7+fail5 fails too.
	if got := mergeRows(pass7, fail5); got.Verdict != VerdictFail {
		t.Fatalf("pass7+fail5 = %q, want fail", got.Verdict)
	}
	never := Row{Target: "t", Verdict: VerdictNever}
	if got := mergeRows(never, pass7); got.Verdict != VerdictPass {
		t.Fatalf("never+pass7 = %q, want pass", got.Verdict)
	}
}

// The Tracker's SetCost / SetStartedAt must be safe under the harness
// pattern of "update as the run progresses", including from goroutines
// (a metrics collector may run off the main test goroutine). The race
// detector is what proves this — this test's value is being run under
// -race with a concurrent SetCost.
func TestTracker_SetCost_ConcurrentIsRaceSafe(t *testing.T) {
	tr := &Tracker{target: "test:live:probe"}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			tr.SetCost(float64(i) * 0.01)
		}
		close(done)
	}()
	for i := 0; i < 200; i++ {
		tr.SetCost(float64(i) * 0.02)
	}
	<-done
}

// A guard's skipLive writes the real prerequisite through RecordSkip, and
// the SAME test's Track cleanup then writes a second `skipped` row for the
// same target. The unnamed fallback must not be unioned next to the named
// reason: a row saying both "no claude CLI: …" and "the harness skipped
// without naming a prerequisite" reads as two contradictory causes, and the
// second is noise the guard already answered.
func TestAGuardsNamedSkipIsNotDilutedByTheFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	t.Setenv(LedgerPathEnv, path)
	t.Setenv(EnvTarget, "test:live:probe")
	resetProcessWritten()

	named := "no claude CLI: set ITERION_LIVE_CLAUDE_CLI or put claude on PATH"
	fake := &fakeTB{}
	Track(fake)             // the test registered its tracker first…
	RecordSkip(fake, named) // …then a guard refused, naming the prerequisite
	fake.skipped = true
	fake.finish() // the tracker's cleanup writes its own (reasonless) skip row

	l, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	row, ok := l.Get("test:live:probe")
	if !ok {
		t.Fatal("no row for the skipped target")
	}
	if row.SkipReason != named {
		t.Fatalf("row skip_reason = %q, want exactly the guard's reason %q", row.SkipReason, named)
	}
	if strings.Contains(row.SkipReason, UnnamedSkipReason) {
		t.Fatalf("the fallback was folded next to the named prerequisite: %q", row.SkipReason)
	}
}

// The fallback still fires when NOTHING named the prerequisite: "skipped"
// with no reason beats `never` with no trace, and the row must say which
// case a reader is in.
func TestAnUnnamedSkipStillCarriesTheFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	t.Setenv(LedgerPathEnv, path)
	t.Setenv(EnvTarget, "test:live:probe")
	resetProcessWritten()

	fake := &fakeTB{}
	Track(fake)
	fake.skipped = true
	fake.finish()

	l, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	row, _ := l.Get("test:live:probe")
	if row.Verdict != VerdictSkipped {
		t.Fatalf("verdict %q, want skipped", row.Verdict)
	}
	if row.SkipReason != UnnamedSkipReason {
		t.Fatalf("row skip_reason = %q, want the fallback — an empty reason would read as `never` with a date", row.SkipReason)
	}
}

// The served_model column says which case a reader is in. What the events
// named wins; otherwise "" is reserved for a run in which NO LLM node ran
// (nothing could have served), and a run in which one ran but the backend
// named nothing — the shape every claude_code / codex / pi re-prove lands
// in, since only the claw path emits llm_request — carries the unobserved
// sentinel, with the spec a re-target asked for. Each case is a different
// receipt, and collapsing any two of them into "" is the false receipt the
// column exists to prevent.
func TestServedModelForSaysWhichCaseTheColumnIsIn(t *testing.T) {
	const asked = "openai_compatible/gpt-oss-120b"
	cases := []struct {
		name   string
		served []string
		llm    int
		asked  string
		want   string
	}{
		{"what the events named wins, folded", []string{"b/y", "a/x", "b/y"}, 2, asked, "a/x,b/y"},
		{"no LLM node ran: nothing could have served", nil, 0, asked, ""},
		{"no LLM node ran, no override: still the empty column", nil, 0, "", ""},
		{"an LLM node ran and nothing named a model", nil, 1, "", ServedModelUnobserved},
		{"an LLM node ran under a re-target that asked for a spec", nil, 3, asked, ServedModelUnobservedPrefix + asked},
		{"the asked-for spec is trimmed", nil, 1, "  " + asked + " ", ServedModelUnobservedPrefix + asked},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ServedModelFor(c.served, c.llm, c.asked); got != c.want {
				t.Fatalf("ServedModelFor(%v, %d, %q) = %q, want %q", c.served, c.llm, c.asked, got, c.want)
			}
		})
	}
	// The sentinel is never a spec a reader could mistake for an answer,
	// and it survives the list fold unsplit: ListSep is a comma and neither
	// a spec nor the sentinel carries one.
	got := UnobservedServedModel(asked)
	if got == asked || got == "" {
		t.Fatalf("UnobservedServedModel(%q) = %q — must be neither the bare spec nor the empty column", asked, got)
	}
	if folded := foldDistinct(got, got, ServedModelUnobserved); folded != ServedModelUnobserved+ListSep+got {
		t.Fatalf("the fold mangled the sentinels: %q", folded)
	}
	// Both shapes are recognised back, so the status screen can render
	// them in words.
	if a, ok := IsUnobservedServedModel(got); !ok || a != asked {
		t.Fatalf("IsUnobservedServedModel(%q) = (%q, %v), want (%q, true)", got, a, ok, asked)
	}
	if a, ok := IsUnobservedServedModel(ServedModelUnobserved); !ok || a != "" {
		t.Fatalf("IsUnobservedServedModel(bare) = (%q, %v), want (\"\", true)", a, ok)
	}
	if _, ok := IsUnobservedServedModel(asked); ok {
		t.Fatalf("IsUnobservedServedModel(%q) recognised a real spec as the sentinel", asked)
	}
}

// The write says where the row went — and the regeneration note (copy the
// ledger over live_seed.json, re-derive the golden) only when the COMMITTED
// ledger moved. A scratch ledger moves nothing the free gate compares, and
// a disabled tracker writes nothing; a note in either case sends the
// operator to copy a file that did not change.
func TestTheWriterSaysWhereTheRowWentAndWhatMoved(t *testing.T) {
	scratch := filepath.Join(t.TempDir(), "ledger.json")
	t.Setenv(LedgerPathEnv, scratch)
	t.Setenv(EnvTarget, "test:live:probe")
	resetProcessWritten()

	fake := &fakeTB{}
	RecordSkip(fake, "no gateway credential")
	var said string
	for _, line := range fake.logs {
		if strings.Contains(line, "recorded in") {
			said = line
		}
	}
	if said == "" {
		t.Fatalf("RecordSkip never said where the skip went: %q", fake.logs)
	}
	if !strings.Contains(said, scratch) || !strings.Contains(said, "scratch") {
		t.Fatalf("the line does not name the scratch ledger it wrote: %q", said)
	}
	if strings.Contains(said, "live_seed.json") {
		t.Fatalf("a scratch write carries the committed ledger's regeneration note: %q", said)
	}

	// Disabled (no target): nothing is written, so nothing is announced.
	t.Setenv(EnvTarget, "")
	quiet := &fakeTB{}
	RecordSkip(quiet, "no gateway credential")
	for _, line := range quiet.logs {
		if strings.Contains(line, "recorded in") {
			t.Fatalf("a disabled tracker announced a row it never wrote: %q", line)
		}
	}

	// The committed path, decided by the predicate alone: this test must not
	// write the checkout's own ledger to prove the note appears there.
	root := repoRoot(t)
	committed := filepath.Join(root, DefaultRelPath)
	if !isCommittedLedgerPath(committed) {
		t.Fatalf("%s is not recognised as the committed ledger", committed)
	}
	if isCommittedLedgerPath(scratch) {
		t.Fatalf("%s is taken for the committed ledger", scratch)
	}
	line := recordedLine("test:live:probe", VerdictSkipped, committed)
	for _, must := range []string{committed, "live_seed.json", "-update-backendcaps", "skipped"} {
		if !strings.Contains(line, must) {
			t.Fatalf("the committed-ledger line lacks %q: %q", must, line)
		}
	}
}
