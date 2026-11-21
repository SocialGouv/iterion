package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/recordcodec"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/valuecodec"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func prepareEvent(ctx context.Context, e *store.Event) (preparedRecord, error) {
	return eventPlan.Prepare(ctx, e)
}
func TestExactRecordOwnedDateProfile(t *testing.T) {
	off := time.FixedZone("test", 7200)
	stamp := instant.In(off).Add(456 * time.Nanosecond)
	e := &store.Event{Type: store.EventArtifactWritten, Timestamp: instant, Data: map[string]any{"version": int(7), "written_at": stamp, "user": payload()}}
	before, _ := json.Marshal(e)
	baseline := *e
	baseline.Data = map[string]any{"written_at": stamp}
	nativeJSON, _ := json.Marshal(baseline)
	nativeBSON, _ := bson.Marshal(baseline)
	var jm map[string]any
	json.Unmarshal(nativeJSON, &jm)
	var bm bson.M
	bson.Unmarshal(nativeBSON, &bm)
	bd, _ := bsonDocumentProbe(bm["data"])
	t.Logf("historical JSON=%T BSON=%T", jm["data"].(map[string]any)["written_at"], bd["written_at"])
	p, err := prepareEvent(t.Context(), e)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"json", "bson"} {
		var got store.Event
		if kind == "json" {
			raw, _ := p.JSON()
			err = eventPlan.DecodeJSON(t.Context(), raw, &got)
		} else {
			raw, _ := p.BSON()
			err = eventPlan.DecodeBSON(t.Context(), raw, &got)
		}
		if err != nil {
			t.Fatal(err)
		}
		if got.Data["written_at"] != stamp.UTC().Format(time.RFC3339Nano) {
			t.Fatal(got.Data)
		}
		t.Logf("%s new written_at=%q", kind, got.Data["written_at"])
	}
	after, _ := json.Marshal(e)
	if !bytes.Equal(before, after) {
		t.Fatal("source profile mutated")
	}
	for _, bad := range []any{&stamp, (*time.Time)(nil), 3, nil, "bad-date", evil(1), time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)} {
		e.Data["written_at"] = bad
		if _, err := prepareEvent(t.Context(), e); err == nil {
			t.Fatalf("invalid control accepted: %T", bad)
		}
	}
	e.Data["written_at"] = stamp
	e.Type = store.EventNodeFinished
	if _, err := prepareEvent(t.Context(), e); !errors.Is(err, valuecodec.ErrUnsupportedValue) {
		t.Fatal("unowned date normalized", err)
	}
	e.Type = store.EventArtifactWritten
	e.Data["user"] = map[string]any{"written_at": stamp}
	if _, err := prepareEvent(t.Context(), e); !errors.Is(err, valuecodec.ErrUnsupportedValue) {
		t.Fatal("nested user date normalized", err)
	}
}
func bsonDocumentProbe(v any) (bson.M, bool) {
	switch d := v.(type) {
	case bson.M:
		return d, true
	case bson.D:
		m := bson.M{}
		for _, e := range d {
			m[e.Key] = e.Value
		}
		return m, true
	}
	return nil, false
}

type recordingEmitter struct{ events []store.Event }

func (e *recordingEmitter) AppendEvent(_ context.Context, _ string, v store.Event) (*store.Event, error) {
	e.events = append(e.events, v)
	return &v, nil
}
func TestExactRecordActualModelHooks(t *testing.T) {
	em := &recordingEmitter{}
	h := model.NewStoreEventHooks(t.Context(), em, "r", iterlog.New(iterlog.LevelTrace, io.Discard), nil, nil)
	h.OnLLMPrompt("n", "system", "user")
	h.OnLLMRequest("n", model.LLMRequestInfo{Model: "test", MessageCount: 2, Timestamp: instant})
	h.OnLLMStepFinish("n", model.LLMStepInfo{Number: 1, Text: "hi", ToolCalls: []model.ToolCallEntry{{Name: "Read", Input: json.RawMessage(`{"big":9007199254740993.0}`)}}})
	h.OnToolStarted("n", model.LLMToolStartedInfo{ToolName: "Read", Input: json.RawMessage(`{"big":9007199254740993.0}`)})
	h.OnToolCall("n", model.LLMToolCallInfo{ToolName: "Read", Output: "ok", Duration: time.Second, Error: errors.New("err")})
	h.OnBackgroundWork("n", model.BackgroundWorkInfo{Tasks: []string{"a", "b"}, WaitedFor: time.Second})
	h.OnUsageCap("n", model.UsageCapInfo{ResetsAt: instant, Percent: 85.5, Cap: 90})
	h.OnProviderFallback("n", model.ProviderFallbackInfo{Cooldown: true, CooldownUntil: instant, Err: errors.New("down")})
	h.OnDelegateFinished("n", model.DelegateInfo{CommandFrontmatterIgnored: []string{"a", "b"}, Duration: time.Second})
	if len(em.events) < 9 {
		t.Fatal("hooks did not emit", len(em.events))
	}
	for i, e := range em.events {
		p, err := prepareEvent(t.Context(), &e)
		if err != nil {
			t.Fatalf("hook %d %s: %v", i, e.Type, err)
		}
		j, err := p.JSON()
		if err != nil {
			t.Fatal(err)
		}
		b, err := p.BSON()
		if err != nil {
			t.Fatal(err)
		}
		var got store.Event
		if err = eventPlan.DecodeJSON(t.Context(), j, &got); err != nil {
			t.Fatal(err)
		}
		var gotB store.Event
		if err = eventPlan.DecodeBSON(t.Context(), b, &gotB); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Data, gotB.Data) {
			t.Fatal("hook JSON/BSON differ")
		}
		t.Logf("hook %s exact JSON/BSON accepted", e.Type)
	}
}
func TestExactRecordOwnedAttemptProfile(t *testing.T) {
	attempt := store.RunAttempt{ID: "digest", StartedAt: instant, WorkflowHash: "workflow", Budget: &store.RunBudget{MaxTokens: 9007199254740993, MaxDuration: "1h"}}
	for _, kind := range []store.EventType{store.EventRunStarted, store.EventRunResumed} {
		e := store.Event{Type: kind, Seq: 0, Timestamp: instant, Data: map[string]any{"attempt": attempt}}
		if _, err := valuecodec.EncodeValue(t.Context(), e.Data); !errors.Is(err, valuecodec.ErrUnsupportedValue) {
			t.Fatal("baseline raw RunAttempt not refused", err)
		}
		p, err := prepareEvent(t.Context(), &e)
		if err != nil {
			t.Fatal(err)
		}
		for _, transport := range []string{"json", "bson"} {
			var got store.Event
			if transport == "json" {
				raw, _ := p.JSON()
				err = eventPlan.DecodeJSON(t.Context(), raw, &got)
			} else {
				raw, _ := p.BSON()
				err = eventPlan.DecodeBSON(t.Context(), raw, &got)
			}
			if err != nil {
				t.Fatal(err)
			}
			builder := runview.NewSnapshotBuilder(&store.Run{ID: "r", CreatedAt: instant})
			builder.Apply(&got)
			snap := builder.Snapshot()
			if len(snap.Run.Attempts) != 1 || snap.Run.Attempts[0].ID != "digest" || snap.Run.Attempts[0].Budget.MaxTokens != 9007199254740993 {
				t.Fatalf("snapshot attempt lost: %#v", snap)
			}
			t.Logf("%s %s actual snapshot consumes normalized attempt", kind, transport)
		}
	}
}
func TestExactRecordAttemptProfileRefusal(t *testing.T) {
	if reflect.TypeFor[store.RunAttempt]().NumField() != 4 || reflect.TypeFor[store.RunBudget]().NumField() != 6 {
		t.Fatal("owned projection must be reviewed for new fields")
	}
	for _, bad := range []store.RunAttempt{{ID: string([]byte{255}), StartedAt: instant}, {ID: "ok", StartedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}, {ID: "ok", StartedAt: instant, Budget: &store.RunBudget{MaxDuration: string([]byte{255})}}} {
		e := store.Event{Type: store.EventRunStarted, Data: map[string]any{"attempt": bad}}
		if _, err := prepareEvent(t.Context(), &e); err == nil {
			t.Fatal("bad control accepted")
		}
	}
	a := store.RunAttempt{ID: "ok", StartedAt: instant}
	e := store.Event{Type: store.EventRunStarted, Data: map[string]any{"attempt": &a}}
	if _, err := prepareEvent(t.Context(), &e); err == nil {
		t.Fatal("pointer silently promoted")
	}
	e.Type = store.EventNodeFinished
	e.Data["attempt"] = a
	if _, err := prepareEvent(t.Context(), &e); !errors.Is(err, valuecodec.ErrUnsupportedValue) {
		t.Fatal("user struct normalized", err)
	}
}

func TestExactRecordTimestampStrict(t *testing.T) {
	for _, s := range []string{"2026-11-03T17:29:01.1230004567Z", "2026-11-03T17:29:01+24:00", "2026-11-03T17:29:01+00:60", "2026-11-03T17:29:01,1Z", "2026-02-30T17:29:01Z"} {
		e := store.Event{Type: store.EventArtifactWritten, Data: map[string]any{"written_at": s}}
		if _, err := prepareEvent(t.Context(), &e); err == nil {
			t.Fatalf("lossy timestamp accepted %q", s)
		}
	}
	for _, s := range []string{"2026-11-03T17:29:01.1230004560Z", "2026-11-03T17:29:01.1000000000000000000Z", "2026-11-03T19:29:01.123000456+02:00", "2026-11-03T17:29:01." + strings.Repeat("0", 100000) + "Z"} {
		e := store.Event{Type: store.EventArtifactWritten, Data: map[string]any{"written_at": s}}
		p, err := prepareEvent(t.Context(), &e)
		if err != nil {
			t.Fatal(err)
		}
		j, _ := p.JSON()
		var got store.Event
		if err = eventPlan.DecodeJSON(t.Context(), j, &got); err != nil {
			t.Fatal(err)
		}
		again, err := prepareEvent(t.Context(), &got)
		if err != nil {
			t.Fatal(err)
		}
		j2, _ := again.JSON()
		if !bytes.Equal(j, j2) {
			t.Fatal("profile not idempotent")
		}
	}
}
func TestExactRecordAttemptProjectionFields(t *testing.T) {
	for _, c := range []struct {
		typ   reflect.Type
		names []string
	}{{reflect.TypeFor[store.RunAttempt](), []string{"ID", "StartedAt", "WorkflowHash", "Budget"}}, {reflect.TypeFor[store.RunBudget](), []string{"MaxCostUSD", "MaxTokens", "WarnTokens", "MaxIterations", "MaxDuration", "MaxParallelBranches"}}} {
		var names []string
		for i := 0; i < c.typ.NumField(); i++ {
			names = append(names, c.typ.Field(i).Name)
		}
		if !reflect.DeepEqual(names, c.names) {
			t.Fatal("owned projection drift", names)
		}
	}
}

type exactProfileCancelContext struct {
	context.Context
	calls int
}

func (c *exactProfileCancelContext) Err() error {
	c.calls++
	if c.calls > 64 {
		return context.Canceled
	}
	return nil
}
func TestExactRecordTimestampCancellation(t *testing.T) {
	ctx := &exactProfileCancelContext{Context: t.Context()}
	e := store.Event{Type: store.EventArtifactWritten, Data: map[string]any{"written_at": "2026-11-03T17:29:01." + strings.Repeat("0", 100000) + "Z"}}
	if _, err := eventPlan.EncodeJSON(ctx, e); !errors.Is(err, context.Canceled) {
		t.Fatal("fraction scan ignored cancellation", err)
	}
}
func TestExactRecordProfileDecodeAtomic(t *testing.T) {
	rawPlan, err := recordcodec.Compile(reflect.TypeFor[store.Event](), "Data")
	if err != nil {
		t.Fatal(err)
	}
	src := store.Event{Type: store.EventArtifactWritten, Data: map[string]any{"written_at": "2026-11-03T17:29:01.1230004567Z"}}
	for _, pair := range []struct {
		enc func(context.Context, any) ([]byte, error)
		dec func(context.Context, []byte, any) error
	}{{rawPlan.EncodeJSON, eventPlan.DecodeJSON}, {rawPlan.EncodeBSON, eventPlan.DecodeBSON}} {
		raw, err := pair.enc(t.Context(), src)
		if err != nil {
			t.Fatal(err)
		}
		dest := store.Event{RunID: "sentinel"}
		if err = pair.dec(t.Context(), raw, &dest); err == nil || dest.RunID != "sentinel" {
			t.Fatal("invalid profile partially published", err)
		}
	}
}

func TestExactRecordAttemptNullRefused(t *testing.T) {
	for _, value := range []any{nil, map[string]any(nil)} {
		e := store.Event{Type: store.EventRunStarted, Data: map[string]any{"attempt": value}}
		if _, err := prepareEvent(t.Context(), &e); err == nil {
			t.Fatal("null attempt encoded but would not decode")
		}
	}
	// An absent key and a non-null materialized native object remain valid.
	for _, data := range []map[string]any{nil, {}, {"attempt": map[string]any{}}} {
		e := store.Event{Type: store.EventRunStarted, Data: data}
		if _, err := prepareEvent(t.Context(), &e); err != nil {
			t.Fatal(err)
		}
	}
}
