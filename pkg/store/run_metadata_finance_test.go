package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRunMetadataFilesystemFinancialFormat(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"acquire", "decision", "reserve", "receipt"} {
		for _, future := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", op, future), func(t *testing.T) {
				s, err := New(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				ctx := t.Context()
				r, err := s.CreateRunWithValueFormat(ctx, RunCreateSpec{ID: "r", Format: 2, Status: RunStatusRunning})
				if err != nil {
					t.Fatal(err)
				}
				authority := RunExecutionAuthority{RunID: r.ID, Epoch: r.ExecutionEpoch}
				expected := RunExecutionExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch, Status: r.Status}
				reservation := OutputCorrectionReservation{ID: "initial", ExecutionEpoch: r.ExecutionEpoch, ScopeKey: r.ID, StartedAt: time.Now()}
				if op == "receipt" {
					if ok, e := s.ReserveInitialUsageIf(ctx, r.ID, authority, reservation); e != nil || !ok {
						t.Fatal(ok, e)
					}
				}
				obs, err := SummarizeUsageComponents("collection", []UsageComponent{{Ordinal: 1, Source: UsageGenericReported, Tokens: 3, CostKnown: true, CostKind: UsageAdditiveCost, CostUSD: 1}})
				if err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(s.runJSONPath(r.ID))
				if err != nil {
					t.Fatal(err)
				}
				if future {
					before = []byte(strings.Replace(string(before), `"format_version": 2`, `"format_version": 99`, 1))
					if err = os.WriteFile(s.runJSONPath(r.ID), before, 0600); err != nil {
						t.Fatal(err)
					}
				}
				var ok bool
				switch op {
				case "acquire":
					_, ok, err = s.AcquireRunExecutionIf(ctx, r.ID, expected, ExecutionStart)
				case "decision":
					ok, err = s.SetRunOutputCorrectionIf(ctx, r.ID, authority, "node", OutputCorrectionExpected{Absent: true}, OutputCorrectionEpisode{Generation: "g", Revision: 1, Status: "active", Budget: 1})
				case "reserve":
					ok, err = s.ReserveInitialUsageIf(ctx, r.ID, authority, reservation)
				case "receipt":
					ok, err = s.RecordOutputCorrectionReceipt(ctx, r.ID, InitialUsageTarget(reservation), OutputCorrectionReceipt{Observation: &obs})
				}
				after, e := os.ReadFile(s.runJSONPath(r.ID))
				if e != nil {
					t.Fatal(e)
				}
				if future {
					if ok || !errors.Is(err, ErrUnsupportedValueFormat) || !bytes.Equal(before, after) {
						t.Fatalf("future applied=%t err=%v changed=%t", ok, err, !bytes.Equal(before, after))
					}
				} else if !ok || err != nil || bytes.Equal(before, after) {
					t.Fatalf("positive applied=%t err=%v changed=%t", ok, err, !bytes.Equal(before, after))
				}
			})
		}
	}
}

func TestRunMetadataFilesystemTwoClientsPreserveDistinctWrites(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if _, err = first.CreateRunWithValueFormat(ctx, RunCreateSpec{ID: "r", Format: 2}); err != nil {
		t.Fatal(err)
	}
	const perClient = 16
	errs := make(chan error, 2*perClient)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for n, s := range []*FilesystemRunStore{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := range perClient {
				id := fmt.Sprintf("client-%d-%d", n, i)
				errs <- s.SetSubbotChild(ctx, "r", id, id)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	r, err := first.LoadRun(ctx, "r")
	if err != nil || len(r.SubbotChildren) != 2*perClient {
		t.Fatalf("lost writes children=%v err=%v", r, err)
	}
}
