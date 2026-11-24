package errtrack

import (
	"context"
	"errors"
	"testing"
)

type graphFixtureBox struct{ Payload any }

func (graphFixtureBox) Error() string { return "box" }

type graphFixtureMany struct{ children []error }

func (graphFixtureMany) Error() string     { return "many" }
func (e graphFixtureMany) Unwrap() []error { return e.children }

type graphFixtureLink struct{ child error }

func (*graphFixtureLink) Error() string   { return "link" }
func (e *graphFixtureLink) Unwrap() error { return e.child }

type graphFixtureOpaque struct{ e error }

func (graphFixtureOpaque) Error() string   { return "opaque" }
func (e graphFixtureOpaque) As(t any) bool { return errors.As(e.e, t) }
func TestErrorGraphDynamicComparability(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(map[bool]string{false: "any", true: "all"}[all], func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("valid error panics: %v", r)
				}
			}()
			e := graphFixtureBox{Payload: []string{"x"}}
			if all {
				if AllLeafCauses(e, func(error) bool { return false }) {
					t.Fatal("unexpected allowed")
				}
			} else if AnyCause(e, func(error) bool { return false }) {
				t.Fatal("unexpected match")
			}
		})
	}
}
func TestErrorGraphTraversal(t *testing.T) {
	yes := context.Canceled
	no := errors.New("no")
	match := func(e error) bool { return e == yes }
	a := &graphFixtureLink{}
	a.child = a
	b := &graphFixtureLink{}
	c := &graphFixtureLink{child: b}
	b.child = c
	for _, cyc := range []error{a, b} {
		if AnyCause(cyc, func(error) bool { return false }) || AllLeafCauses(cyc, func(error) bool { return true }) {
			t.Fatal("cycle accepted")
		}
	}
	shared := &graphFixtureLink{child: yes}
	for _, e := range []error{graphFixtureOpaque{yes}, errors.Join(shared, shared), graphFixtureMany{[]error{nil, yes}}} {
		if !AnyCause(e, func(e error) bool {
			_, ok := e.(interface{ Unwrap() []error })
			if ok {
				return false
			}
			return match(e)
		}) || !AllLeafCauses(e, match) {
			t.Fatal("observable allowed graph lost")
		}
	}
	if AllLeafCauses(errors.Join(yes, no), match) || AllLeafCauses(graphFixtureMany{}, match) || AllLeafCauses(nil, match) {
		t.Fatal("fail-open leaf traversal")
	}
}

type graphFixtureAs struct{ mode string }

func (graphFixtureAs) Error() string { return "as" }
func (e graphFixtureAs) As(t any) bool {
	switch e.mode {
	case "false":
		return false
	case "nil":
		return true
	case "self":
		if p, ok := t.(*error); ok {
			*p = e
			return true
		}
	}
	return false
}
func TestErrorGraphOpaqueContracts(t *testing.T) {
	for _, mode := range []string{"false", "nil", "self"} {
		e := graphFixtureAs{mode}
		if AnyCause(e, func(error) bool { return false }) || AllLeafCauses(e, func(error) bool { return false }) {
			t.Fatalf("unobserved cause accepted %s", mode)
		}
	}
}
