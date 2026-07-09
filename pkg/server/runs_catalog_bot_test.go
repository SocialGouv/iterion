package server

import "testing"

// TestInferCatalogBotID pins the path→bot-id inference that lets a cloud
// launch/resume reference a catalog bundle without uploading its bytes. The
// security-relevant cases are the ones that must return "" so the strict
// cloud "source required" gate still fires for an arbitrary operator path.
func TestInferCatalogBotID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Catalog-shaped paths → the bundle dir name.
		{"bots/whats-next/main.bot", "whats-next"},
		{"/opt/iterion/bots/whats-next/main.bot", "whats-next"},
		{"examples/foo/main.bot", "foo"},
		{"./bots/review-pr/main.bot", "review-pr"},
		{"whats-next/main.bot", "whats-next"},
		// Bare names / loose basenames → candidate id (confirmed later
		// against the real catalog by catalogBotSource).
		{"whats-next", "whats-next"},
		{"hello.bot", "hello"},
		// Arbitrary absolute/workspace paths with no bots|examples segment
		// → "" so cloud still demands inline source (the security gate).
		{"/home/operator/secret/plan.bot", ""},
		{"/etc/passwd", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := inferCatalogBotID(c.in); got != c.want {
			t.Errorf("inferCatalogBotID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
