package storekit

import (
	"errors"
	"sort"
	"testing"
)

type item struct {
	ID    string
	Owner string
	Tag   string
	N     int
}

var (
	errMiss = errors.New("miss")
	errDup  = errors.New("dup")
)

func newTestMemory() *Memory[item] { return NewMemory[item](errMiss) }

func TestMemory_InsertAndGet(t *testing.T) {
	m := newTestMemory()
	if err := m.Insert("a", item{ID: "a", N: 1}, errDup); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := m.Insert("a", item{ID: "a", N: 2}, errDup); !errors.Is(err, errDup) {
		t.Fatalf("duplicate Insert err = %v, want errDup", err)
	}
	got, err := m.Get("a")
	if err != nil || got.N != 1 {
		t.Fatalf("Get = %+v, %v (duplicate insert must not overwrite)", got, err)
	}
	if _, err := m.Get("ghost"); !errors.Is(err, errMiss) {
		t.Fatalf("Get(ghost) err = %v, want errMiss", err)
	}
}

func TestMemory_InsertUnless(t *testing.T) {
	m := newTestMemory()
	conflictTag := func(tag string) func(item) bool {
		return func(e item) bool { return tag != "" && e.Tag == tag }
	}
	if err := m.InsertUnless("a", item{ID: "a", Tag: "k1"}, conflictTag("k1"), errDup); err != nil {
		t.Fatalf("first InsertUnless: %v", err)
	}
	// Secondary-key conflict under a different primary id.
	if err := m.InsertUnless("b", item{ID: "b", Tag: "k1"}, conflictTag("k1"), errDup); !errors.Is(err, errDup) {
		t.Fatalf("conflicting InsertUnless err = %v, want errDup", err)
	}
	if _, err := m.Get("b"); !errors.Is(err, errMiss) {
		t.Fatal("conflicting InsertUnless must not write")
	}
	// No conflict (empty tag) → same primary id overwrites.
	if err := m.InsertUnless("a", item{ID: "a", N: 9}, conflictTag(""), errDup); err != nil {
		t.Fatalf("overwrite InsertUnless: %v", err)
	}
	if got, _ := m.Get("a"); got.N != 9 {
		t.Fatalf("overwrite not applied: %+v", got)
	}
}

func TestMemory_PutFindList(t *testing.T) {
	m := newTestMemory()
	m.Put("a", item{ID: "a", Owner: "u1", Tag: "h1"})
	m.Put("b", item{ID: "b", Owner: "u1"})
	m.Put("c", item{ID: "c", Owner: "u2"})
	m.Put("a", item{ID: "a", Owner: "u1", Tag: "h1", N: 5}) // upsert

	got, err := m.Find(func(e item) bool { return e.Tag == "h1" })
	if err != nil || got.N != 5 {
		t.Fatalf("Find = %+v, %v", got, err)
	}
	if _, err := m.Find(func(e item) bool { return e.Tag == "nope" }); !errors.Is(err, errMiss) {
		t.Fatalf("Find miss err = %v, want errMiss", err)
	}

	out := m.List(func(e item) bool { return e.Owner == "u1" })
	if len(out) != 2 {
		t.Fatalf("List u1 = %d items, want 2", len(out))
	}
	ids := []string{out[0].ID, out[1].ID}
	sort.Strings(ids)
	if ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("List u1 ids = %v", ids)
	}
	if out := m.List(func(e item) bool { return e.Owner == "nope" }); out != nil {
		t.Fatalf("empty List = %v, want nil", out)
	}
}

func TestMemory_ReplaceMutateDelete(t *testing.T) {
	m := newTestMemory()
	m.Put("a", item{ID: "a", N: 1})

	if err := m.Replace("a", item{ID: "a", N: 2}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if err := m.Replace("ghost", item{}); !errors.Is(err, errMiss) {
		t.Fatalf("Replace(ghost) err = %v, want errMiss", err)
	}

	committed, err := m.Mutate("a", func(e *item) bool { e.N++; return true })
	if err != nil || !committed {
		t.Fatalf("Mutate commit = %v, %v", committed, err)
	}
	committed, err = m.Mutate("a", func(e *item) bool { e.N = 99; return false })
	if err != nil || committed {
		t.Fatalf("Mutate abort = %v, %v", committed, err)
	}
	if got, _ := m.Get("a"); got.N != 3 {
		t.Fatalf("post-mutate N = %d, want 3 (abort must not write)", got.N)
	}
	if _, err := m.Mutate("ghost", func(*item) bool { return true }); !errors.Is(err, errMiss) {
		t.Fatalf("Mutate(ghost) err = %v, want errMiss", err)
	}

	if err := m.Delete("a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := m.Delete("a"); !errors.Is(err, errMiss) {
		t.Fatalf("second Delete err = %v, want errMiss", err)
	}
}

func TestMemory_DeleteWhere(t *testing.T) {
	m := newTestMemory()
	m.Put("a", item{ID: "a", Owner: "u1"})
	m.Put("b", item{ID: "b", Owner: "u1"})
	m.Put("c", item{ID: "c", Owner: "u2"})
	m.DeleteWhere(func(e item) bool { return e.Owner == "u1" })
	if out := m.List(func(item) bool { return true }); len(out) != 1 || out[0].ID != "c" {
		t.Fatalf("post-DeleteWhere = %+v, want only c", out)
	}
	m.DeleteWhere(func(item) bool { return false }) // no match is not an error
}
