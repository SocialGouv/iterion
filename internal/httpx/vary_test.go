package httpx

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func varyOf(rec *httptest.ResponseRecorder) string {
	return strings.Join(rec.Header().Values("Vary"), ", ")
}

func TestAddVaryKeepsWhatCameBefore(t *testing.T) {
	// The whole point: a handler that owns "Origin" must not discard the token
	// a middleware upstream already recorded. Set() did exactly that, and the
	// loss is invisible — the response still looks right, it is merely
	// cacheable across a dimension it varies on.
	rec := httptest.NewRecorder()
	AddVary(rec, "X-Forwarded-Host")
	AddVary(rec, "Origin")

	got := varyOf(rec)
	for _, want := range []string{"X-Forwarded-Host", "Origin"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Vary = %q, lost %q", got, want)
		}
	}
}

func TestAddVaryIsIdempotent(t *testing.T) {
	rec := httptest.NewRecorder()
	AddVary(rec, "Origin")
	AddVary(rec, "origin") // a second handler, different capitalisation
	AddVary(rec, "Origin")

	if n := strings.Count(strings.ToLower(varyOf(rec)), "origin"); n != 1 {
		t.Fatalf("Vary = %q, want Origin exactly once, got %d", varyOf(rec), n)
	}
}

func TestAddVaryAddsOnlyTheMissingTokens(t *testing.T) {
	rec := httptest.NewRecorder()
	AddVary(rec, "Accept")
	AddVary(rec, "X-Forwarded-Host", "Accept", "Sec-Fetch-Dest")

	got := strings.ToLower(varyOf(rec))
	if strings.Count(got, "accept") != 1 {
		t.Fatalf("Vary = %q, duplicated Accept", varyOf(rec))
	}
	for _, want := range []string{"x-forwarded-host", "sec-fetch-dest"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Vary = %q, missing %q", varyOf(rec), want)
		}
	}
}

func TestAddVaryIgnoresEmptyFields(t *testing.T) {
	rec := httptest.NewRecorder()
	AddVary(rec, "", "  ")
	if got := varyOf(rec); got != "" {
		t.Fatalf("Vary = %q, want none", got)
	}
}
