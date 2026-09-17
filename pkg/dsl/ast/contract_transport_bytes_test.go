package ast_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// A JSON value travels as bytes, and the transport's nil-element walk does
// not visit them one by one — a 16 MiB default would otherwise cost one
// formatted path string per byte, and the read would take seconds.
//
// The witness is the SHAPE of the cost against the payload's size, not the
// cost itself: read the same document at two sizes 16x apart and require
// what it costs to stay put. A walk that visits the bytes cannot — its work
// is one allocation per byte by construction — and a walk that does not is
// flat whatever the machine is doing, which is what a wall-clock ratio on a
// shared runner could not promise (#1338: 10.4x against a 10x threshold, on
// a PR touching nothing here).
//
// Measured, with the byte-slice guard in rejectNilElements and without it:
//
//	payload     4 KiB     64 KiB
//	guarded       127        132   (+5)
//	walked     12_168    196_500   (+184_332)
//
// The budget below sits two orders of magnitude clear of both.
func TestUnmarshalFileDoesNotWalkAJSONValueByteByByte(t *testing.T) {
	const (
		small = 4 << 10
		large = 64 << 10
		// Room for the decoder's own growth — bigger buffers cost a few
		// more grow-allocations — and nothing like the 61_440 bytes added
		// between the two sizes, each of which a byte walk would pay for.
		budget = 1000
	)

	// A JSON value (the contract input's default) is what the walk must not
	// enter. The same bytes in a plain string field never reach it, and are
	// the control that proves the fixture grew at all.
	document := func(kind string, n int) []byte {
		big := strings.Repeat("x", n)
		if kind == "value" {
			return []byte(`{"contracts":[{"name":"c","inputs":[{"name":"x","type":"string","default":"` + big + `"}]}]}`)
		}
		return []byte(`{"contracts":[{"name":"c","responsibility":"` + big + `"}]}`)
	}
	allocs := func(raw []byte) float64 {
		return testing.AllocsPerRun(3, func() {
			f, err := ast.UnmarshalFile(raw)
			if err != nil {
				t.Fatal(err)
			}
			if len(f.Contracts) != 1 {
				t.Fatal("the contract was not read")
			}
		})
	}

	valueSmall, valueLarge := allocs(document("value", small)), allocs(document("value", large))
	if grew := valueLarge - valueSmall; grew > budget {
		t.Errorf("reading a JSON value cost %.0f more allocations at %d bytes than at %d (%.0f against %.0f): the nil walk visits the bytes",
			grew, large, small, valueLarge, valueSmall)
	}

	// The control: the same bytes where no walk goes. If this ever grows,
	// the witness above has stopped measuring the walk and is measuring the
	// decoder, and its budget means nothing.
	//
	// Stated rather than implied: no defect in the walk can turn this one
	// red — that is what makes it the control — so it is falsified only by
	// a change in the decoder. Both assertions were seen to fire, on live
	// numbers, by running this test once with a negative budget.
	stringSmall, stringLarge := allocs(document("string", small)), allocs(document("string", large))
	if grew := stringLarge - stringSmall; grew > budget {
		t.Errorf("the control grew by %.0f allocations (%.0f against %.0f): the decoder itself now scales with the payload, so this test no longer isolates the walk",
			grew, stringLarge, stringSmall)
	}

	// And the value comes back whole: a walk that is skipped must not be a
	// value that is dropped.
	f, err := ast.UnmarshalFile(document("value", large))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(f.Contracts[0].Inputs[0].Default), large+2; got != want {
		t.Fatalf("the default came back %d bytes long, want %d", got, want)
	}
}
