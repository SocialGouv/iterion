package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestIncarnationTurnsRestartAttemptsAndFallback(t *testing.T) {
	for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			s, ctx := directoryFixture(t, format)
			const node = "nœud with spaces"
			for _, model := range []string{"first", "second"} {
				cp := &TurnCheckpoint{RunID: "r", NodeID: node, LoopIter: 2, TurnIndex: 3, Model: model, Messages: json.RawMessage("opaque " + model), MessagesRef: "ref"}
				if err := s.WriteTurn(ctx, cp); err != nil {
					t.Fatal(err)
				}
			}
			restarted, err := New(s.root)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := restarted.ListTurns(ctx, "r", node, 2)
			if err != nil || len(rows) != 2 {
				t.Fatal(rows, err)
			}
			if rows[0].Model != "first" || !rows[0].Superseded || rows[0].Attempt != 1 || rows[1].Model != "second" || rows[1].Attempt != 2 || rows[1].Superseded {
				t.Fatalf("attempts changed: %+v %+v", rows[0], rows[1])
			}
			check := func(cp *TurnCheckpoint, err error) {
				t.Helper()
				if err != nil || cp == nil || cp.Model != "second" || cp.Attempt != 2 {
					t.Fatal(cp, err)
				}
			}
			check(restarted.LoadTurn(ctx, "r", node, 2, 3))
			check(restarted.LatestTurn(ctx, "r", node))
			check(restarted.LoadTurnAtIndex(ctx, "r", node, 3))
			raw, err := restarted.LoadTurnMessages(ctx, "r", node, 2, 3)
			if err != nil || string(raw) != "opaque second" {
				t.Fatal(string(raw), err)
			}
			path, err := s.runPayloadPath(ctx, "r", "turns", node, "index.json")
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
				t.Fatal(err)
			}
			check(restarted.LatestTurn(ctx, "r", node))
			check(restarted.LoadTurnAtIndex(ctx, "r", node, 3))
			old, err := s.runPayloadPath(ctx, "r", "turns", node, "2", "3.attempt-1.messages.json")
			if err != nil {
				t.Fatal(err)
			}
			raw, err = os.ReadFile(old)
			if err != nil || string(raw) != "opaque first" {
				t.Fatal(string(raw), err)
			}
		})
	}
}

func TestIncarnationTurnsRefuseChildLinks(t *testing.T) {
	for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
		for _, level := range []string{"node", "iteration"} {
			for _, op := range []string{"write", "load", "list", "latest", "at-index", "messages"} {
				t.Run(fmt.Sprintf("%d/%s/%s", format, level, op), func(t *testing.T) {
					s, ctx := directoryFixture(t, format)
					cp := &TurnCheckpoint{RunID: "r", NodeID: "node", LoopIter: 0, TurnIndex: 0, Model: "original", Messages: json.RawMessage("opaque")}
					if err := s.WriteTurn(ctx, cp); err != nil {
						t.Fatal(err)
					}
					parts := []string{"turns", "node"}
					if level == "iteration" {
						parts = append(parts, "0")
					}
					path, err := s.runPayloadPath(ctx, "r", parts...)
					if err != nil {
						t.Fatal(err)
					}
					if err = os.Rename(path, path+"-held"); err != nil {
						t.Fatal(err)
					}
					if err = os.Symlink(path+"-held", path); err != nil {
						t.Fatal(err)
					}
					switch op {
					case "write":
						cp.Model = "changed"
						err = s.WriteTurn(ctx, cp)
					case "load":
						_, err = s.LoadTurn(ctx, "r", "node", 0, 0)
					case "list":
						_, err = s.ListTurns(ctx, "r", "node", 0)
					case "latest":
						_, err = s.LatestTurn(ctx, "r", "node")
					case "at-index":
						_, err = s.LoadTurnAtIndex(ctx, "r", "node", 0)
					case "messages":
						_, err = s.LoadTurnMessages(ctx, "r", "node", 0, 0)
					}
					if err == nil {
						t.Fatal("child symlink accepted")
					}
					held := path + "-held"
					if level == "node" {
						held = filepath.Join(held, "0")
					}
					raw, e := os.ReadFile(filepath.Join(held, "0.json"))
					if e != nil {
						t.Fatal(e)
					}
					var got TurnCheckpoint
					if e = json.Unmarshal(raw, &got); e != nil || got.Model != "original" {
						t.Fatal(got, e)
					}
				})
			}
		}
	}
}
