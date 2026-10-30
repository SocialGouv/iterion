package model

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// turnFieldsKeptAsProduced names the far-side fields of relayedTurnCapture
// that reach disk as the container wrote them, each because a reader needs the
// value itself:
//
//   - session_id — the key a fork resumes the backend session with
//     (store.TurnCheckpoint.SessionID → BackendSessionID, pkg/runview/fork.go);
//     a rewritten one resumes nothing.
//   - backend — the same fork picks the child's backend from it and gates the
//     conversation replay on `turn.Backend == delegate.BackendClaw`.
//
// It is an exception list, not a description: the test below drives the secret
// through EVERY string field the wire form declares and demands that a field
// absent from this list never reaches disk in clear. A field added to
// relayedTurnCapture is therefore covered the day it is added, and a field
// removed from this list has to be redacted before the suite goes green.
var turnFieldsKeptAsProduced = map[string]string{
	"session_id": "the key a fork resumes the backend session with",
	"backend":    "the backend a fork re-launches the child with",
}

// relayedTurnWireStrings lists the json names of every string-typed field of
// relayedTurnCapture — read from the TYPE, so the next field added to the wire
// form joins this witness without anyone remembering to add it.
func relayedTurnWireStrings(t *testing.T) []string {
	t.Helper()
	rt := reflect.TypeOf(relayedTurnCapture{})
	var names []string
	for i := range rt.NumField() {
		f := rt.Field(i)
		if f.Type.Kind() != reflect.String {
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			t.Fatalf("relayedTurnCapture.%s is a string with no json name — the witness cannot drive it", f.Name)
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		t.Fatal("scenario broken: relayedTurnCapture declares no string field — the witness drives nothing")
	}
	return names
}

// A relayed turn capture is the one relayed event whose payload is PERSISTED
// rather than logged: the launcher's hook writes a store.TurnCheckpoint the
// per-node timeline and the Fork API serve. Layer 0 wraps the event sink, not
// the turn sink, so what the container put in that record reaches disk through
// this hook alone.
//
// Driven through the real chain — the crossing's event handler, the relay
// decoder, the production store hooks, the filesystem store — with the secret
// moved to ONE field per subtest and the verdict read off the WHOLE tree the
// run wrote. A field the exception list does not name must come back
// redacted; a field it names must come back in clear, so the list cannot
// quietly outlive its reason either.
func TestClawRelay_PersistsNoRelayedTurnInClear(t *testing.T) {
	t.Parallel()
	const secret = "sk-proj-TURNWITNESS0123456789abcdefghij"

	// What a field is allowed to look like once the run has written it.
	const (
		// redacted — the pass rewrote the value: the secret is gone and the
		// placeholder is on disk. The DEFAULT, so a field nobody thought
		// about reddens rather than passing vacuously.
		redacted = iota
		// keptAsProduced — on turnFieldsKeptAsProduced, with its reader.
		keptAsProduced
		// digested — the record keeps a sha256 of the value, never the value:
		// stronger than the pass, and neither secret nor placeholder lands.
		digested
	)

	type subject struct {
		name    string
		put     map[string]any
		verdict int
	}
	subjects := []subject{
		{
			name: "tool_calls[0].name",
			put: map[string]any{"tool_calls": []any{map[string]any{
				"name": "mcp_deploy_push(token=" + secret + ")", "input": map[string]any{},
			}}},
		},
		{
			name: "tool_calls[0].input",
			put: map[string]any{"tool_calls": []any{map[string]any{
				"name": "mcp_deploy_push", "input": map[string]any{"body": secret},
			}}},
		},
		{
			// The snapshot a fork replays, decoded as []api.Message before
			// the hook sees it — a shape the decoder refuses is a turn that
			// never reaches the store, which the LoadTurn below catches.
			name: "conversation",
			put: map[string]any{"conversation": []any{map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "text", "text": secret}},
			}}},
		},
	}
	// Every string field of the wire form, from the type itself.
	for _, field := range relayedTurnWireStrings(t) {
		v := redacted
		switch {
		case turnFieldsKeptAsProduced[field] != "":
			v = keptAsProduced
		case field == "text":
			v = digested
		}
		subjects = append(subjects, subject{name: field, put: map[string]any{field: secret}, verdict: v})
	}

	for _, s := range subjects {
		t.Run(s.name, func(t *testing.T) {
			const runID = "run-turn-redaction"
			g := secretguard.New([]secretguard.Secret{{Name: "OPENAI_API_KEY", Value: secret}}, secretguard.DefaultConfig())
			placeholder := g.Redact(secret)
			if placeholder == secret || placeholder == "" {
				t.Fatalf("scenario broken: the guard does not redact the witness value (%q)", placeholder)
			}

			root := t.TempDir()
			st, err := store.New(root)
			if err != nil {
				t.Fatalf("store: %v", err)
			}
			var runLog strings.Builder
			logger := iterlog.New(iterlog.LevelInfo, &runLog)
			hooks := NewStoreEventHooks(context.Background(), st, runID, logger, g, nil)
			b := NewClawBackend(NewRegistry(), hooks, RetryPolicy{}, WithClawLogger(logger))

			// A turn the fork path would accept, so no subtest proves its
			// point on a record the hook refused to write.
			payload := map[string]any{"step": 1, "iteration": 0, "backend": "claw"}
			for k, v := range s.put {
				payload[k] = v
			}
			handler, _ := b.multiplexerHandler(context.Background(), delegate.Task{NodeID: "campaign", Iteration: 0}, sessionSeed{})
			handler.OnEvent(relayTurnCaptureType, payload)

			if _, err := st.LoadTurn(context.Background(), runID, "campaign", 0, 0); err != nil {
				t.Fatalf("scenario broken: the relayed turn was not persisted: %v", err)
			}

			var withSecret, withPlaceholder []string
			if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				body, rerr := os.ReadFile(path)
				if rerr != nil {
					return rerr
				}
				if strings.Contains(string(body), secret) {
					withSecret = append(withSecret, path)
				}
				if strings.Contains(string(body), placeholder) {
					withPlaceholder = append(withPlaceholder, path)
				}
				return nil
			}); err != nil {
				t.Fatalf("walk the run's tree: %v", err)
			}

			switch s.verdict {
			case keptAsProduced:
				// Asserted in the POSITIVE direction too, so the exception
				// list cannot outlive its reason: a field that starts coming
				// back redacted has to leave the list.
				if len(withSecret) == 0 {
					t.Errorf("%s is on the exception list (%s) but the run no longer writes it in clear — redact it and drop the entry",
						s.name, turnFieldsKeptAsProduced[s.name])
				}
			case digested:
				if len(withSecret) > 0 {
					t.Errorf("a relayed turn's %s reached disk with the run's secret in clear, in %v", s.name, withSecret)
				}
				if digest := sha256Hex(secret); !contains(root, digest, t) {
					t.Errorf("%s is meant to reach disk as a digest, and the run wrote neither the value nor sha256(%s)", s.name, s.name)
				}
			default:
				if len(withSecret) > 0 {
					t.Errorf("a relayed turn's %s reached disk with the run's secret in clear, in %v", s.name, withSecret)
				}
				if len(withPlaceholder) == 0 {
					t.Errorf("nothing the run wrote carries the placeholder for %s — the walk proves nothing", s.name)
				}
			}
		})
	}
}

// contains reports whether any file under root holds want. Used for the digest
// arm, where the proof is a value the record DERIVED rather than one it kept.
func contains(root, want string, t *testing.T) bool {
	t.Helper()
	found := false
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(body), want) {
			found = true
		}
		return nil
	}); err != nil {
		t.Fatalf("walk the run's tree: %v", err)
	}
	return found
}
