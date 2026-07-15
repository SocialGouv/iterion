package store

import (
	"context"
	"strings"
	"testing"
)

func TestNormalizeTags(t *testing.T) {
	tests := []struct {
		name    string
		in      []string
		want    []string
		wantErr bool
	}{
		{"nil is empty", nil, []string{}, false},
		{"trim and drop empty", []string{"  release ", "", "   "}, []string{"release"}, false},
		{"dedup preserves order", []string{"a", "b", "a", "c", "b"}, []string{"a", "b", "c"}, false},
		{"case sensitive", []string{"Release", "release"}, []string{"Release", "release"}, false},
		{"max len ok", []string{strings.Repeat("x", MaxTagLen)}, []string{strings.Repeat("x", MaxTagLen)}, false},
		{"too long", []string{strings.Repeat("x", MaxTagLen+1)}, nil, true},
		{"too many", tooManyTags(MaxTagsPerRun + 1), nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeTags(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("NormalizeTags(%v) = %v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeTags(%v) unexpected error: %v", tc.in, err)
			}
			if !equalStrings(got, tc.want) {
				t.Errorf("NormalizeTags(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestFilesystemRunTagsRoundTrip(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()

	// Unset run returns empty, never nil, never 404.
	got, err := s.GetRunTags(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetRunTags (empty): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("GetRunTags on unset run = %v, want empty", got)
	}

	want := []string{"release", "flaky"}
	if err := s.SetRunTags(ctx, "run-1", want); err != nil {
		t.Fatalf("SetRunTags: %v", err)
	}
	got, err = s.GetRunTags(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetRunTags: %v", err)
	}
	if !equalStrings(got, want) {
		t.Errorf("GetRunTags = %v, want %v", got, want)
	}

	// Overwrite replaces the whole set (not merge).
	if err := s.SetRunTags(ctx, "run-1", []string{"customer-x"}); err != nil {
		t.Fatalf("SetRunTags overwrite: %v", err)
	}
	got, _ = s.GetRunTags(ctx, "run-1")
	if !equalStrings(got, []string{"customer-x"}) {
		t.Errorf("after overwrite = %v, want [customer-x]", got)
	}

	// Clearing to empty persists an empty set.
	if err := s.SetRunTags(ctx, "run-1", nil); err != nil {
		t.Fatalf("SetRunTags clear: %v", err)
	}
	got, _ = s.GetRunTags(ctx, "run-1")
	if len(got) != 0 {
		t.Errorf("after clear = %v, want empty", got)
	}
}

func TestAsRunTagStore(t *testing.T) {
	if AsRunTagStore(nil) != nil {
		t.Error("AsRunTagStore(nil) should be nil")
	}
	if AsRunTagStore(tmpStore(t)) == nil {
		t.Error("filesystem store should satisfy RunTagStore")
	}
}

func tooManyTags(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "tag" + strings.Repeat("x", 1) + itoa(i)
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
