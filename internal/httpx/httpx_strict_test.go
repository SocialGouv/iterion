package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type strictNested struct {
	Depth int `json:"depth"`
}

type strictEmbedded struct {
	Inherited string `json:"inherited"`
}

type strictTarget struct {
	strictEmbedded
	Vars   map[string]string `json:"vars,omitempty"`
	BotID  string            `json:"bot_id,omitempty"`
	Nested strictNested      `json:"nested,omitempty"`
	Secret string            `json:"-"`
	hidden string            //nolint:unused // an unexported field is not a name a caller may send
}

// The measured failure, in the shape it arrived: a parameter sent under a
// name the struct does not declare. Dropped, it is indistinguishable from
// one that was honoured — the request is accepted and the value does
// nothing. Refused, the caller reads the answer instead of the behaviour.
func TestDecodeJSONStrict_RefusesAnUnknownFieldAndNamesTheAcceptedOnes(t *testing.T) {
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"inputs":{"adversarial":"false"}}`))
	var dst strictTarget
	err := DecodeJSONStrict(r, &dst)
	if err == nil {
		t.Fatal("an undeclared field was accepted — the caller cannot tell it did nothing")
	}
	if !strings.Contains(err.Error(), "inputs") {
		t.Errorf("the error must name the field it refused: %v", err)
	}
	// The list is the whole point: `inputs` and `vars` are semantically
	// related and textually unrelated, so a "did you mean" by string
	// distance would not have offered the one the caller wanted.
	if !strings.Contains(err.Error(), "vars") {
		t.Errorf("the error must name what IS accepted: %v", err)
	}
}

// The other half. A body of declared fields decodes exactly as before —
// including through an embedded struct — or the guard is a wall, not a door.
func TestDecodeJSONStrict_AcceptsWhatTheStructDeclares(t *testing.T) {
	r := httptest.NewRequest("POST", "/",
		strings.NewReader(`{"vars":{"adversarial":"false"},"bot_id":"golden-master","inherited":"x","nested":{"depth":2}}`))
	var dst strictTarget
	if err := DecodeJSONStrict(r, &dst); err != nil {
		t.Fatalf("a well-formed body was refused: %v", err)
	}
	if dst.Vars["adversarial"] != "false" || dst.BotID != "golden-master" ||
		dst.Inherited != "x" || dst.Nested.Depth != 2 {
		t.Fatalf("decoded shape = %+v", dst)
	}
}

// Unknown fields nest: a parameter misspelled INSIDE a declared object is
// dropped by exactly the same mechanism.
func TestDecodeJSONStrict_RefusesAnUnknownFieldInsideADeclaredObject(t *testing.T) {
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"nested":{"deph":2}}`))
	var dst strictTarget
	if err := DecodeJSONStrict(r, &dst); err == nil {
		t.Fatal("a misspelled nested field was accepted")
	}
}

// What the caller may send is what the struct offers — no more.
func TestAcceptedFields_OffersOnlyWhatACallerMaySend(t *testing.T) {
	got := acceptedFields(&strictTarget{})
	for _, want := range []string{"vars", "bot_id", "nested", "inherited"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing from %q", want, got)
		}
	}
	if strings.Contains(got, "Secret") || strings.Contains(got, "hidden") {
		t.Errorf("a field no caller may send was offered: %q", got)
	}
	if got != "bot_id, inherited, nested, vars" {
		t.Errorf("names = %q, want them sorted and complete", got)
	}
}

// One value, then nothing. A decoder stops at the end of the first value,
// so a body carrying a second one would be accepted with its second half
// silently dropped — a PATCH whose `repos` rode the second object would
// answer 200 and restrict nothing. Trailing whitespace is not content.
func TestDecodeJSONStrict_RefusesContentAfterTheValue(t *testing.T) {
	for _, body := range []string{`{"bot_id":"a"} {"vars":{"x":"y"}}`, "{\"bot_id\":\"a\"}\n{\"vars\":{\"x\":\"y\"}}", `{"bot_id":"a"} garbage`, `{"bot_id":"a"}]`} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		var dst strictTarget
		if err := DecodeJSONStrict(r, &dst); err == nil {
			t.Errorf("%q accepted — its tail was dropped without a word", body)
		}
	}
	for _, body := range []string{`{"bot_id":"a"}`, "{\"bot_id\":\"a\"}\n", "  {\"bot_id\":\"a\"}  \n\t"} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		var dst strictTarget
		if err := DecodeJSONStrict(r, &dst); err != nil || dst.BotID != "a" {
			t.Errorf("%q refused: %v", body, err)
		}
	}
}

// A body over the cap is refused, not cut: a decoder fed the first 10 MB of
// a longer body would decode whatever fits and drop the rest — a second
// value, or the tail of the first — without a word.
func TestDecodeJSON_RefusesABodyOverTheCap(t *testing.T) {
	padding := strings.Repeat(" ", maxBody)
	for name, decode := range map[string]func(*http.Request, any) error{"lenient": DecodeJSON, "strict": DecodeJSONStrict} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(`{"bot_id":"a"}`+padding+`{"vars":{"x":"y"}}`))
		var dst strictTarget
		if err := decode(r, &dst); err == nil {
			t.Errorf("%s: a body over the cap was accepted with its tail dropped", name)
		}
		r = httptest.NewRequest("POST", "/", strings.NewReader(`{"bot_id":"a"}`+padding[:maxBody-len(`{"bot_id":"a"}`)]))
		dst = strictTarget{}
		if err := decode(r, &dst); err != nil || dst.BotID != "a" {
			t.Errorf("%s: a body exactly at the cap was refused: %v", name, err)
		}
	}
}
