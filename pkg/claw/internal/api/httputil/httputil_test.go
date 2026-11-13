package httputil

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

func TestTruncateBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		max  int
		want string
	}{
		{"shorter than budget", "hello", 10, "hello"},
		{"exact budget", "hello", 5, "hello"},
		{"longer than budget", "hello world", 5, "hello…"},
		{"multibyte", "héllo", 4, "héll…"},
		{"empty", "", 5, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TruncateBody(tc.body, tc.max); got != tc.want {
				t.Errorf("TruncateBody(%q,%d) = %q, want %q", tc.body, tc.max, got, tc.want)
			}
		})
	}
}

func TestExtractText(t *testing.T) {
	cases := []struct {
		name   string
		blocks []api.ContentBlock
		want   string
	}{
		{"empty", nil, ""},
		{"single text", []api.ContentBlock{{Type: "text", Text: "hello"}}, "hello"},
		{
			"multi text concatenates with newline",
			[]api.ContentBlock{
				{Type: "text", Text: "a"},
				{Type: "text", Text: "b"},
			},
			"a\nb",
		},
		{
			"skips blocks with empty text",
			[]api.ContentBlock{
				{Type: "text", Text: "a"},
				{Type: "tool_use", Name: "x"},
				{Type: "text", Text: "b"},
			},
			"a\nb",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractText(tc.blocks); got != tc.want {
				t.Errorf("ExtractText = %q, want %q", got, tc.want)
			}
		})
	}
}
