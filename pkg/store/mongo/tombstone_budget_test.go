package mongo

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// Tombstone gate for the run doc's BUDGET carriers (deterministic, no
// Mongo needed): DeleteRun promises to "permanently remove a run and all
// of its data", and it keeps that promise by $unset-ing the run doc's
// payload fields rather than dropping the document (the skeleton is what
// makes a late writer get ErrRunDeleted instead of resurrecting the run).
//
// A budget carrier that is added to store.Run but forgotten in that
// $unset list survives deletion forever — the run reads as deleted while
// still holding the operator's declared caps. That is exactly what
// happened to `budget_overrides`: it shipped alongside `budget`,
// `budget_raises` and `model_overrides`, all three of which were already
// stripped, and it alone was missed.
//
// Scope is deliberately narrow — the budget family only, derived from
// store.Run's own bson tags. It cannot go red on an unrelated field
// someone adds elsewhere on the run doc; it goes red exactly when the
// next `budget*` carrier repeats this omission.
func TestTombstoneStripsEveryBudgetCarrier(t *testing.T) {
	unset := tombstoneUnsetKeys(t)

	rt := reflect.TypeOf(store.Run{})
	found := 0
	for i := 0; i < rt.NumField(); i++ {
		name := bsonFieldName(rt.Field(i))
		if !strings.HasPrefix(name, "budget") {
			continue
		}
		found++
		if !unset[name] {
			t.Errorf("store.Run field %q (bson %q) is a budget carrier but is NOT stripped by DeleteRun's tombstone $unset (pkg/store/mongo/runs.go). "+
				"Add %q to that map, or a deleted run keeps holding the caps it was launched with.",
				rt.Field(i).Name, name, name)
		}
	}
	if found == 0 {
		t.Fatal("no budget* bson fields found on store.Run — parser drifted; fix this test")
	}
}

// tombstoneUnsetKeys reads the keys of DeleteRun's tombstone $unset map
// out of this package's own source. Scoped to the `tomb := …` literal so
// the OTHER $unset in this file (UpdateRunStatus's finished_at) cannot
// vacuously satisfy the assertion.
func tombstoneUnsetKeys(t *testing.T) map[string]bool {
	t.Helper()
	src := readSource(t, "runs.go")
	lit := regexp.MustCompile(`(?s)tomb := bson\.M\{.*?\}\}`).FindString(src)
	if lit == "" {
		t.Fatal("DeleteRun's `tomb := bson.M{…}` literal not found in runs.go — parser drifted; fix this test")
	}
	unsetPart := lit[strings.Index(lit, `"$unset"`):]
	if !strings.HasPrefix(unsetPart, `"$unset"`) {
		t.Fatal("no $unset section in DeleteRun's tombstone literal — parser drifted; fix this test")
	}
	keys := map[string]bool{}
	for _, m := range regexp.MustCompile(`"([a-z0-9_]+)":\s*""`).FindAllStringSubmatch(unsetPart, -1) {
		keys[m[1]] = true
	}
	if len(keys) == 0 {
		t.Fatal("no $unset keys parsed from DeleteRun's tombstone literal — parser drifted; fix this test")
	}
	return keys
}

// bsonFieldName is the field's persisted key: the bson tag's name when
// present, else Go's default (the lowercased field name).
func bsonFieldName(f reflect.StructField) string {
	tag := f.Tag.Get("bson")
	if tag == "" {
		return strings.ToLower(f.Name)
	}
	name := strings.Split(tag, ",")[0]
	if name == "" || name == "-" {
		return strings.ToLower(f.Name)
	}
	return name
}
