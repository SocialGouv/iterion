package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReviewR53TurnIncumbentRedirect(t *testing.T) {
	for _, field := range []string{"node", "iter", "turn", "run"} {
		t.Run(field, func(t *testing.T) {
			s, e := New(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			ctx := context.Background()
			old := &TurnCheckpoint{RunID: "r", NodeID: "node", TextDigest: "old", Messages: []byte("old opaque")}
			if e = s.WriteTurn(ctx, old); e != nil {
				t.Fatal(e)
			}
			key := filepath.Join(s.root, "runs", "r", "turns", "node", "0", "0.json")
			raw, e := os.ReadFile(key)
			if e != nil {
				t.Fatal(e)
			}
			var corrupt TurnCheckpoint
			if e = json.Unmarshal(raw, &corrupt); e != nil {
				t.Fatal(e)
			}
			switch field {
			case "node":
				corrupt.NodeID = "victim"
			case "iter":
				corrupt.LoopIter = 2
			case "turn":
				corrupt.TurnIndex = 3
			case "run":
				corrupt.RunID = "foreign"
			}
			target := filepath.Join(s.root, "runs", "r", "turns", corrupt.NodeID, fmt.Sprint(corrupt.LoopIter), fmt.Sprint(corrupt.TurnIndex)+".messages.json")
			if e = os.MkdirAll(filepath.Dir(target), 0700); e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(target, []byte("victim"), 0600); e != nil {
				t.Fatal(e)
			}
			raw, e = json.Marshal(corrupt)
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(key, raw, 0600); e != nil {
				t.Fatal(e)
			}
			e = s.WriteTurn(ctx, &TurnCheckpoint{RunID: "r", NodeID: "node", TextDigest: "new"})
			got, re := os.ReadFile(target)
			if e == nil || re != nil || string(got) != "victim" {
				t.Fatalf("incumbent metadata accepted; error=%v victim=%q read=%v", e, got, re)
			}
		})
	}
}
func TestReviewR53TurnInvalidInputNoSupersede(t *testing.T) {
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	old := &TurnCheckpoint{RunID: "r", NodeID: "node", TextDigest: "old", Messages: []byte("opaque old")}
	if e = s.WriteTurn(ctx, old); e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(s.root, "runs", "r", "turns", "node", "0", "0.messages.json")
	e = s.WriteTurn(ctx, &TurnCheckpoint{RunID: "r", NodeID: "node", TextDigest: "new", WrittenAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)})
	if e == nil {
		t.Fatal("invalid timestamp accepted")
	}
	raw, re := os.ReadFile(p)
	if re != nil || string(raw) != "opaque old" {
		t.Fatalf("invalid incoming turn already moved incumbent messages: %q %v", raw, re)
	}
}
func TestReviewR53TurnOpaqueMessagesAndCorruptIndex(t *testing.T) {
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if e = s.WriteTurn(ctx, &TurnCheckpoint{RunID: "r", NodeID: "node", LoopIter: i, TurnIndex: i, TextDigest: fmt.Sprint(i), Messages: []byte("not JSON")}); e != nil {
			t.Fatal(e)
		}
	}
	if e = os.WriteFile(filepath.Join(s.root, "runs", "r", "turns", "node", "index.json"), []byte("{"), 0600); e != nil {
		t.Fatal(e)
	}
	turn, e := s.LatestTurn(ctx, "r", "node")
	if e != nil || turn.LoopIter != 1 || turn.TurnIndex != 1 {
		t.Fatal(turn, e)
	}
	turn, e = s.LoadTurnAtIndex(ctx, "r", "node", 0)
	if e != nil || turn.LoopIter != 0 {
		t.Fatal(turn, e)
	}
	raw, e := s.LoadTurnMessages(ctx, "r", "node", 1, 1)
	if e != nil || string(raw) != "not JSON" {
		t.Fatal(string(raw), e)
	}
}
