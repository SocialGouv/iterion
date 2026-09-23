package parser

import (
	"reflect"
	"strings"
	"testing"
)

// A list that is not written as one — a value with no `[`, a line ended on
// a dangling comma — is said where it stands and takes its line: the NEXT
// property is read as itself, never as the list's elements. Every element
// reader consumes at least one token, so the line end must never reach one.
func TestAListWithoutItsBracketNeverReadsTheNextProperty(t *testing.T) {
	nl := string(rune(10))
	head := strings.Join([]string{"workflow w:", "  entry: done", "  sandbox:"}, nl) + nl
	for _, c := range []struct {
		name, mounts string
		wantMounts   []string
		wantMessage  string
	}{
		{"a value with no bracket", `    mounts: "/x:/x"`, nil, "expected ["},
		{"a dangling comma", `    mounts: ["/x:/x",`, []string{"/x:/x"}, "expected ] to close the list"},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := Parse("x.bot", head+c.mounts+nl+`    image: "img"`+nl)
			sb := res.File.Workflows[0].Sandbox
			if sb == nil || sb.Image != "img" {
				t.Fatalf("the next property was read as elements: sandbox = %+v, diagnostics %v", sb, res.Diagnostics)
			}
			if !reflect.DeepEqual(sb.Mounts, c.wantMounts) {
				t.Errorf("mounts = %q, want %q", sb.Mounts, c.wantMounts)
			}
			if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, c.wantMessage) || res.Diagnostics[0].Line != 4 {
				t.Errorf("want one diagnostic on line 4 saying %q, got %v", c.wantMessage, res.Diagnostics)
			}
		})
	}
}

// An empty string is neither a rule nor a mount: the sandbox would refuse it
// only when it starts. The reader says it where it stands and leaves it out,
// in both written forms, and the rest of the list is read.
func TestAnEmptyStringIsNotARuleOrAMount(t *testing.T) {
	nl := string(rune(10))
	head := strings.Join([]string{"workflow w:", "  entry: done", "  sandbox:"}, nl) + nl
	for _, c := range []struct {
		name, mounts string
		wantMounts   []string
	}{
		{"inline", `    mounts: ["/x:/x", ""]` + nl, []string{"/x:/x"}},
		{"dash form", `    mounts:` + nl + `      - "/x:/x"` + nl + `      - ""` + nl, []string{"/x:/x"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := Parse("x.bot", head+c.mounts)
			sb := res.File.Workflows[0].Sandbox
			if sb == nil || !reflect.DeepEqual(sb.Mounts, c.wantMounts) {
				t.Fatalf("mounts = %+v, want %q; diagnostics %v", sb, c.wantMounts, res.Diagnostics)
			}
			if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, "empty string") {
				t.Errorf("want one diagnostic naming the empty string, got %v", res.Diagnostics)
			}
		})
	}
}
