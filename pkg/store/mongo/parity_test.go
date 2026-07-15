package mongo

import (
	"os"
	"regexp"
	"testing"
)

// Cloud-parity gate (deterministic, no Mongo needed): every per-run Mongo
// collection MUST be swept by DeleteRun, or a deleted run's data lingers
// until its TTL (or forever, for a TTL-less collection) — a silent leak
// and a broken DeleteRun contract. This exact gap shipped once (run_logs
// was omitted from the children list, caught only by an out-of-band audit
// — commit c1324a196). This test is the guard so it can't recur: add a
// new col* collection and forget the DeleteRun wiring → red CI, in the
// same PR, before review.
//
// The check is a static parse of this package's own source:
//   - every `colXxx = "name"` constant in store.go is a collection this
//     store owns;
//   - DeleteRun (runs.go) sweeps a `children` slice of `{"name", …}` rows;
//   - a per-run collection must appear in that slice.
//
// nonPerRunCollections are the only collections exempt: `runs` is the run
// document itself (DeleteRun removes it via a direct DeleteOne, not the
// children sweep). Anything else added here needs an explicit reason.
var nonPerRunCollections = map[string]bool{
	"runs": true, // the run doc; deleted by DeleteRun's own DeleteOne
}

func TestDeleteRunCoversEveryPerRunCollection(t *testing.T) {
	storeSrc := readSource(t, "store.go")
	runsSrc := readSource(t, "runs.go")

	// Collection names this store declares: `colX = "name"`.
	colRe := regexp.MustCompile(`\bcol[A-Za-z0-9_]*\s*=\s*"([a-z0-9_]+)"`)
	declared := map[string]bool{}
	for _, m := range colRe.FindAllStringSubmatch(storeSrc, -1) {
		declared[m[1]] = true
	}
	if len(declared) == 0 {
		t.Fatal("no col* collection constants found in store.go — parser drifted; fix this test")
	}

	// Names swept by DeleteRun: the `{"name", s.field}` rows of the
	// children slice. Grep the whole file for `{"name", s.` rows — the
	// children slice is the only construct of that shape here.
	childRe := regexp.MustCompile(`\{"([a-z0-9_]+)",\s*s\.[A-Za-z0-9_]+\}`)
	swept := map[string]bool{}
	for _, m := range childRe.FindAllStringSubmatch(runsSrc, -1) {
		swept[m[1]] = true
	}
	if len(swept) == 0 {
		t.Fatal("no DeleteRun children rows found in runs.go — parser drifted; fix this test")
	}

	for name := range declared {
		if nonPerRunCollections[name] {
			continue
		}
		if !swept[name] {
			t.Errorf("collection %q is declared but NOT swept by DeleteRun (runs.go children slice). "+
				"Add {%q, s.<field>} to the children list, or exempt it in nonPerRunCollections with a reason. "+
				"Without this, deleting a run leaks its %q data.", name, name, name)
		}
	}
}

func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}
