package parser

import (
	"strings"
	"testing"
)

// Profile 2 keeps a paragraph break: CanonicalPromptBodyIn(2, …) is where
// the profile-2 lexer settles — pinned to the lexer the same way the
// profile-1 form is, over the same body shapes, with the header in front.
func TestCanonicalPromptBodyInProfileTwoIsWhereTheLexerSettles(t *testing.T) {
	for _, body := range promptBodyShapes {
		if err := CheckPromptBody(body); err != nil {
			t.Errorf("%q: a writable body was refused: %v", body, err)
			continue
		}
		want := CanonicalPromptBodyIn(2, body)
		if again := CanonicalPromptBodyIn(2, want); again != want {
			t.Errorf("%q: the canonical form is not a fixed point: %q -> %q", body, want, again)
		}
		once, diags := parseBody(t, "dsl: 2\n"+indentBody(body))
		for _, d := range diags {
			t.Errorf("%q: written raw, unexpected diagnostic: %s", body, d.Error())
		}
		twice, diags := parseBody(t, "dsl: 2\n"+indentBody(once))
		for _, d := range diags {
			t.Errorf("%q: written after one pass, unexpected diagnostic: %s", body, d.Error())
		}
		if twice != want {
			t.Errorf("%q: the profile-2 lexer settles on %q, CanonicalPromptBodyIn says %q", body, twice, want)
		}
		got, diags := parseBody(t, "dsl: 2\n"+indentBody(want))
		for _, d := range diags {
			t.Errorf("%q: the canonical form does not parse: %s", body, d.Error())
		}
		if got != want {
			t.Errorf("%q: the canonical form %q reads back as %q", body, want, got)
		}
	}
}

// The difference between the profiles is exactly the paragraph break:
// interior blank lines survive in profile 2 and nothing else changes —
// leading and trailing blank lines are still dropped, a body still never
// ends with a newline, the first line still sets the indentation.
func TestProfileTwoKeepsParagraphBreaksAndNothingElse(t *testing.T) {
	src := "prompt p:\n\n  # Title\n\n  First paragraph\n  continues.\n   \n\n  Second paragraph.\n\n\nagent a:\n  description: \"x\"\n"
	v1, diags := parseBody(t, src)
	if len(diags) != 0 {
		t.Fatalf("profile 1: %v", diags)
	}
	if v1 != "# Title\nFirst paragraph\ncontinues.\nSecond paragraph." {
		t.Fatalf("profile 1 body: %q", v1)
	}
	v2, diags := parseBody(t, "dsl: 2\n"+src)
	if len(diags) != 0 {
		t.Fatalf("profile 2: %v", diags)
	}
	if v2 != "# Title\n\nFirst paragraph\ncontinues.\n\n\nSecond paragraph." {
		t.Fatalf("profile 2 body: %q", v2)
	}
	if strings.HasSuffix(v2, "\n") {
		t.Fatalf("a body never ends with a newline: %q", v2)
	}
	if CanonicalPromptBodyIn(1, "a\n\nb") != "a\nb" || CanonicalPromptBodyIn(2, "a\n\nb") != "a\n\nb" {
		t.Fatalf("CanonicalPromptBodyIn does not switch on the profile")
	}
}
