package parser

import (
	"slices"
	"testing"
)

// The queue must never hand back a token already read: a splice landing in
// the materialized-but-unread region while the head segment is half consumed
// (a lookahead backed up into the prefix) used to leave the head's consumed
// prefix linked, and the pop that reached the segment served it again — the
// DEDENT spliced twice came out three times, and queued's count no longer
// matched the chain. The repro the #2880 review round 2 proved: splice,
// consume one token of the queue, Backup, splice at the cursor — every token
// of the resulting stream is served exactly once.
func TestSpliceQueuePrependDropsTheConsumedHeadPrefix(t *testing.T) {
	l := lexSource("q.bot", "agent a:\n  model: \"m\"\n  backend: claw\n", sourceBudget())
	orig := slices.Clone(l.tokens)
	dedA := []Token{{Type: TokenDedent, Line: 1000, Column: 1}, {Type: TokenDedent, Line: 1000, Column: 1}}
	dedB := []Token{{Type: TokenDedent, Line: 2000, Column: 1}}
	l.Next() // ti = 1
	const at = 3
	l.splice(at, dedA) // queue: dedA, dedA, then the tail
	for l.ti < at {
		l.Next()
	}
	l.Next()   // one token of the queue read: the head segment is half consumed
	l.Backup() // and the cursor steps back into the materialized prefix
	l.splice(l.ti, dedB)
	want := append([]Token{dedB[0], dedA[0], dedA[1]}, orig[at:]...)
	var got []Token
	for i := 0; ; i++ {
		if i > len(want)+8 {
			t.Fatalf("the stream does not end: served %v…", got)
		}
		tok := l.Next()
		got = append(got, tok)
		if tok.Type == TokenEOF {
			break
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the queue served %v, want every token exactly once: %v", got, want)
	}
	if l.ahead.queued != 0 {
		t.Fatalf("the count holds %d tokens the chain no longer has", l.ahead.queued)
	}
}

// An empty splice links no segment: an empty one would pop as a head whose
// toks hold nothing — the pop reads past it and panics — and would sit in
// the chain invisible to the count. spliceDedents never asks for one (n <= 0
// returns early); the primitive refuses it too.
func TestSpliceQueueEmptyInsertLinksNoSegment(t *testing.T) {
	l := lexSource("q.bot", "agent a:\n  model: \"m\"\n", sourceBudget())
	orig := slices.Clone(l.tokens)
	l.splice(len(l.tokens), nil)
	if l.ahead.head != nil {
		t.Fatalf("an empty splice linked a segment: %d queued", l.ahead.queued)
	}
	// And into a live queue: the stream is the one the real splice wrote,
	// no more.
	ded := []Token{{Type: TokenDedent, Line: 1000, Column: 1}}
	l.splice(1, ded)
	l.splice(len(l.tokens)+1, nil)
	want := append([]Token{orig[0], ded[0]}, orig[1:]...)
	var got []Token
	for i := 0; ; i++ {
		if i > len(want)+8 {
			t.Fatalf("the stream does not end: served %v…", got)
		}
		tok := l.Next()
		got = append(got, tok)
		if tok.Type == TokenEOF {
			break
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the queue served %v, want %v", got, want)
	}
}
