package appidprobe

import "testing"

func TestDouble(t *testing.T) {
	// BUG (intentional, for App-identity validation): expects 5, but Double(2)=4.
	if got := Double(2); got != 5 {
		t.Fatalf("Double(2) = %d, want 5", got)
	}
}
