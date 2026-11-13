package strutil

import "testing"

func TestASCIIToLower(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Hello WORLD", "hello world"},
		{"Über Straße", "Über straße"},
		{"", ""},
		{"already lowercase", "already lowercase"},
		{"ALL CAPS 123!@#", "all caps 123!@#"},
		{"MiXeD", "mixed"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := ASCIIToLower(tt.input); got != tt.want {
				t.Errorf("ASCIIToLower(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
