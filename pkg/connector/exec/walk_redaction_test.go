package exec

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAWalkReadsItsProtocolKeysThroughTheSameRedaction.
//
// The redaction renames every decoded-body KEY that carries the credential —
// including the package's own protocol keys. A walk that then reads its
// cursor by the ORIGINAL name misses the field, reads the cursor as absent,
// and reports the collection COMPLETE: the silent truncation the Complete
// flag exists to prevent, arriving through the redaction rather than through
// a bound. The reader resolves its field names through the same scrub the
// body went through, so writer and reader agree whatever the credential.
//
// A credential of a few characters inside an ordinary field name is the
// degenerate case redactData's own doc describes; this is the walk's answer
// to it.
func TestAWalkReadsItsProtocolKeysThroughTheSameRedaction(t *testing.T) {
	// "ex" occurs inside BOTH protocol keys: "next" and "items". One rename
	// alone would not prove the reader follows the redaction — the cursor
	// read is what turns a miss into a false complete.
	const credential = "ex"

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests == 1 {
			_, _ = fmt.Fprint(w, `{"items":[{"id":1}],"next":"p2"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"items":[{"id":2}],"next":""}`)
	}))
	defer srv.Close()

	e := &Executor{Client: MarkGuarded(srv.Client())}
	pkg, op := walkFixture(srv.URL, 50)
	items, complete, last, err := e.CallPaged(context.Background(), pkg, op, nil, Credential{SchemeID: "token", Value: credential})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if requests != 2 {
		t.Errorf("the vendor was called %d times, want 2 — the cursor read must survive the redaction renaming its key", requests)
	}
	if len(items) != 2 {
		t.Errorf("items = %d, want the pages actually walked", len(items))
	}
	if !complete {
		t.Error("the vendor said the collection ended (empty cursor); the walk must report complete")
	}
	if last.Requests != 2 {
		t.Errorf("Requests = %d, want the walk's total", last.Requests)
	}
}
