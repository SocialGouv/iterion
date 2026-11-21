package store

import (
	"context"
	"errors"
	"testing"
)

func TestMissingHeaderCallClassRetainsRequestedIdentity(t *testing.T) {
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	bind := func(id, birth string, protocol RunDataProtocol) context.Context {
		b, e := CaptureRunDataBinding(RunDataHeader{RunID: id, Format: LegacyValueFormat, Protocol: protocol, BirthID: birth})
		if e != nil {
			t.Fatal(e)
		}
		c, e := WithRunDataBinding(t.Context(), b)
		if e != nil {
			t.Fatal(e)
		}
		return c
	}
	cancelctx, cancel := context.WithCancel(t.Context())
	cancel()
	cases := []struct {
		name string
		ctx  context.Context
		want error
	}{{"unbound", t.Context(), nil}, {"same-absent", bind("orphan", "", HistoricalRunData), nil}, {"foreign-absent", bind("foreign", "", HistoricalRunData), ErrRunDataIdentityChanged}, {"foreign-birth", bind("foreign", "birth", HistoricalRunData), ErrRunDataIdentityChanged}, {"retired", bind("orphan", "retired", HistoricalRunData), ErrRunDataIdentityChanged}, {"protocol-one", bind("orphan", "strict", IncarnationRunData), ErrRunNotFound}, {"cancelled", cancelctx, context.Canceled}}
	calls := map[string]func(context.Context) error{
		"helper":       func(c context.Context) error { return CheckMissingRunDataParent(c, "orphan") },
		"payload-path": func(c context.Context) error { _, e := s.runPayloadPath(c, "orphan", "messages"); return e },
		"value-record": func(c context.Context) error { _, _, e := s.valueRecordFormatLocked(c, "orphan", false); return e },
		"replacement": func(c context.Context) error {
			return CheckRunDataReplacement(c, nil, &Run{ID: "orphan", FormatVersion: 1})
		},
	}
	for _, tc := range cases {
		for name, call := range calls {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				e := call(tc.ctx)
				if tc.want == nil {
					if e != nil {
						t.Fatal(e)
					}
				} else if !errors.Is(e, tc.want) {
					t.Fatalf("got %v want %v", e, tc.want)
				}
			})
		}
	}
}
