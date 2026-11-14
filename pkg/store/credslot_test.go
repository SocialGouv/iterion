package store

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

// The filesystem twin of the credential-slot ledger, on the parts the
// cross-backend conformance row cannot reach: the race itself (N callers of
// one process asking for K slots at once) and the grace a holder with no run
// document keeps, which needs the holder's own instant moved.

func credSlotRun(t *testing.T, s *FilesystemRunStore, id string, status RunStatus, fps ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, id, "demo", nil); err != nil {
		t.Fatalf("CreateRun %s: %v", id, err)
	}
	r, err := s.LoadRun(ctx, id)
	if err != nil {
		t.Fatalf("LoadRun %s: %v", id, err)
	}
	r.Status = status
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun %s: %v", id, err)
	}
	if len(fps) > 0 {
		if err := s.SetRunCredStamp(ctx, id, RunCredStamp{Fingerprints: fps}); err != nil {
			t.Fatalf("SetRunCredStamp %s: %v", id, err)
		}
	}
}

// Of N runs asking at once for K slots of one key, exactly K are admitted —
// the property the walk's own count cannot give (it reads the run documents
// before the run is written, so every racer reads the same count). No racer
// has a run document yet, as a launch reserves before it persists: none is
// adopted by another's admission, and none is evictable inside the grace.
func TestCredSlotLedger_ConcurrentReservationsAdmitExactlyTheCap(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const fp, max, racers = "fp-capped", 3, 40
	ids := make([]string, racers)
	for i := range ids {
		ids[i] = "racer-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted, refused := 0, 0
	heldSeen := map[int]bool{}
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			adm, err := s.ReserveCredSlot(context.Background(), fp, id, max)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("ReserveCredSlot(%s): %v", id, err)
				return
			}
			heldSeen[adm.Held] = true
			if adm.Granted {
				granted++
			} else {
				refused++
			}
		}(id)
	}
	close(start)
	wg.Wait()
	if granted != max || refused != racers-max {
		t.Fatalf("granted=%d refused=%d, want exactly %d granted of %d racers: the ledger admitted past the cap", granted, refused, max, racers)
	}
	for held := range heldSeen {
		if held > max {
			t.Fatalf("an admission reported %d holders on a cap of %d", held, max)
		}
	}
}

// A holder whose run document is missing keeps its slot for CredSlotGrace —
// a launch reserves before it persists — and loses it once the grace has
// passed: the document is not coming, the launch died between its writes.
func TestCredSlotLedger_MissingRunDocumentKeepsItsSlotForTheGraceOnly(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	const fp = "fp-capped"
	if adm, err := s.ReserveCredSlot(ctx, fp, "ghost", 1); err != nil || !adm.Granted {
		t.Fatalf("ghost reservation: %+v %v", adm, err)
	}
	credSlotRun(t, s, "live", RunStatusRunning, fp)
	if adm, err := s.ReserveCredSlot(ctx, fp, "live", 1); err != nil || adm.Granted {
		t.Fatalf("a holder reserved moments ago with no document yet was evicted: %+v %v", adm, err)
	}
	// Age the ghost's reservation past the grace: the next refusal evicts it.
	s.credSlotMu.Lock()
	s.credSlots[fp][0].at = time.Now().UTC().Add(-CredSlotGrace - time.Second)
	s.credSlotMu.Unlock()
	if adm, err := s.ReserveCredSlot(ctx, fp, "live", 1); err != nil || !adm.Granted || adm.Held != 1 {
		t.Fatalf("a holder with no document past the grace kept its slot: %+v %v", adm, err)
	}
}

// The eviction rule both twins read, case by case.
func TestCredSlotHolderStale(t *testing.T) {
	now := time.Now().UTC()
	fresh, old := now.Add(-time.Second), now.Add(-CredSlotGrace-time.Second)
	spending := &Run{Status: RunStatusRunning, CredFingerprints: []string{"fp"}}
	idle := &Run{Status: RunStatusRunning, CredFingerprints: []string{"fp"}, LLMIdleSince: &now}
	parked := &Run{Status: RunStatusFailedResumable, CredFingerprints: []string{"fp"}}
	restamped := &Run{Status: RunStatusRunning, CredFingerprints: []string{"other"}}
	cases := []struct {
		name  string
		run   *Run
		err   error
		at    time.Time
		stale bool
	}{
		{"spending holder stays", spending, nil, old, false},
		// A lent slot has no path back at the next model node: idle keeps it.
		{"model-idle holder stays", idle, nil, old, false},
		{"parked holder goes", parked, nil, old, true},
		{"re-stamped holder goes", restamped, nil, old, true},
		{"missing document inside the grace stays", nil, ErrRunNotFound, fresh, false},
		{"missing document past the grace goes", nil, ErrRunNotFound, old, true},
		{"deleted document past the grace goes", nil, ErrRunDeleted, old, true},
		{"a store that cannot answer keeps the holder", nil, errors.New("timeout"), old, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CredSlotHolderStale(c.run, c.err, c.at, now, "fp"); got != c.stale {
				t.Fatalf("stale=%v, want %v", got, c.stale)
			}
		})
	}
}

// A gateway's capacity subject is one row whatever the spelling of the host
// a team's key carries: hostnames compare without case, and two keys on one
// gateway written `LiteLLM.example` and `litellm.example` must hold one
// subject's places, not two. No host names no subject — never a bare `gw:`
// row every unresolved gateway would share.
func TestGatewayCapacitySubject(t *testing.T) {
	for _, c := range []struct{ host, want string }{
		{"litellm.example", "gw:litellm.example"},
		{"LiteLLM.Example", "gw:litellm.example"},
		// The absolute spelling dials the same name.
		{"litellm.example.", "gw:litellm.example"},
		{"LiteLLM.Example.", "gw:litellm.example"},
		// An internationalised name and its punycode are one dial name.
		{"bücher.example", "gw:xn--bcher-kva.example"},
		{"xn--bcher-kva.example", "gw:xn--bcher-kva.example"},
		{"BÜCHER.example", "gw:xn--bcher-kva.example"},
		{"10.0.0.7", "gw:10.0.0.7"},
		// Address literals fold to their canonical text.
		{"2001:db8::1", "gw:2001:db8::1"},
		{"2001:DB8:0:0:0:0:0:1", "gw:2001:db8::1"},
		{"::ffff:10.0.0.7", "gw:10.0.0.7"},
		// No dial name, no subject.
		{"", ""},
		{".", ""},
		{"bü\u00a0cher.example", ""},
	} {
		if got := GatewayCapacitySubject(c.host); got != c.want {
			t.Errorf("GatewayCapacitySubject(%q) = %q, want %q", c.host, got, c.want)
		}
	}
}

// Every field of RunCredStamp reaches the run document through Apply, the
// writer the launch path and the filesystem twin of SetRunCredStamp share: a
// field added to the stamp and forgotten there would stamp resumes on Mongo
// and leave every launch unstamped. Each field is set alone and must land in
// its own destination and nowhere else — measured against what Apply writes
// for an empty stamp, so an unconditional write cannot hide a dropped field
// and two crossed fields fail both rows.
func TestRunCredStampApplyWritesEveryField(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	dest := map[string]string{
		"Fingerprints":     "CredFingerprints",
		"CapacitySubjects": "CapacitySubjects",
		"Tiers":            "CredentialTiers",
		"SkippedReopensAt": "SkippedCredReopensAt",
	}
	var base Run
	RunCredStamp{}.Apply(&base)
	typ := reflect.TypeOf(RunCredStamp{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		t.Run(f.Name, func(t *testing.T) {
			to, ok := dest[f.Name]
			if !ok {
				t.Fatalf("RunCredStamp.%s has no destination in this guard — name the Run field Apply writes it to", f.Name)
			}
			var stamp RunCredStamp
			v := reflect.ValueOf(&stamp).Elem().Field(i)
			switch v.Interface().(type) {
			case []string:
				v.Set(reflect.ValueOf([]string{"value-" + f.Name}))
			case *time.Time:
				v.Set(reflect.ValueOf(&at))
			default:
				t.Fatalf("RunCredStamp.%s has type %s, which this guard does not fill — teach it", f.Name, f.Type)
			}
			var r Run
			stamp.Apply(&r)
			got := reflect.ValueOf(&r).Elem().FieldByName(to)
			if !got.IsValid() {
				t.Fatalf("RunCredStamp.%s: this guard names Run.%s, which does not exist — fix the destination", f.Name, to)
			}
			if !reflect.DeepEqual(got.Interface(), v.Interface()) {
				t.Fatalf("RunCredStamp.%s: Run.%s = %v after Apply, want %v", f.Name, to, got.Interface(), v.Interface())
			}
			// Put the destination back to the empty stamp's value: anything
			// left differing is a write the field should not have made.
			got.Set(reflect.ValueOf(&base).Elem().FieldByName(to))
			if !reflect.DeepEqual(r, base) {
				t.Fatalf("RunCredStamp.%s reached a Run field other than %s", f.Name, to)
			}
		})
	}
	// A re-stamp is a fresh attempt: the model-idle marker goes.
	r := Run{LLMIdleSince: &at}
	RunCredStamp{}.Apply(&r)
	if r.LLMIdleSince != nil {
		t.Fatal("Apply kept LLMIdleSince — both twins clear it on a stamp")
	}
}

// The stamp as a launch writes it — every field at once, several values each
// — over a zero run and over a run that already carries another resolution's
// stamp, then an empty stamp over that run. Field by field alone (above), a
// write conditional on another field, an append, a truncation or a skipped
// clear all pass; here each fails its own destination.
func TestRunCredStampApplyReplacesAndClearsTheWholeUnit(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	old := time.Unix(1_600_000_000, 0).UTC()
	dest := map[string]string{
		"Fingerprints":     "CredFingerprints",
		"CapacitySubjects": "CapacitySubjects",
		"Tiers":            "CredentialTiers",
		"SkippedReopensAt": "SkippedCredReopensAt",
	}
	full := RunCredStamp{
		Fingerprints:     []string{"fp-a", "fp-b"},
		CapacitySubjects: []string{"gw:a.example", "gw:b.example"},
		Tiers:            []string{"tier-a", "tier-b"},
		SkippedReopensAt: &at,
	}
	prev := Run{
		CredFingerprints:     []string{"fp-old"},
		CapacitySubjects:     []string{"gw:old.example"},
		CredentialTiers:      []string{"tier-old"},
		SkippedCredReopensAt: &old,
		LLMIdleSince:         &old,
	}
	var empty Run
	RunCredStamp{}.Apply(&empty)
	for _, start := range []struct {
		name string
		run  Run
	}{{"zero run", Run{}}, {"stamped run", prev}} {
		r := start.run
		full.Apply(&r)
		sv := reflect.ValueOf(full)
		for i := 0; i < sv.NumField(); i++ {
			f := sv.Type().Field(i)
			to, ok := dest[f.Name]
			if !ok {
				t.Fatalf("RunCredStamp.%s has no destination in this guard", f.Name)
			}
			got := reflect.ValueOf(r).FieldByName(to)
			if !got.IsValid() {
				t.Fatalf("this guard names Run.%s, which does not exist", to)
			}
			if !reflect.DeepEqual(got.Interface(), sv.Field(i).Interface()) {
				t.Errorf("%s, whole stamp: Run.%s = %v, want %v", start.name, to, got.Interface(), sv.Field(i).Interface())
			}
		}
		if r.LLMIdleSince != nil {
			t.Errorf("%s, whole stamp: LLMIdleSince kept", start.name)
		}
		RunCredStamp{}.Apply(&r)
		if !reflect.DeepEqual(r, empty) {
			t.Errorf("%s: an empty stamp over a stamped run left %+v, want every destination cleared", start.name, r)
		}
	}
}
