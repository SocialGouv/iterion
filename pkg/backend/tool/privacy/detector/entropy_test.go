package detector

import "testing"

func TestEntropy_Empty(t *testing.T) {
	if got := shannonEntropy(""); got != 0 {
		t.Fatalf("entropy(\"\") = %v, want 0", got)
	}
}

func TestEntropy_Calibration(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantMin float64
		wantMax float64
	}{
		{"weak_password", "password", 2.0, 3.5},
		{"changeme", "changeme", 2.0, 3.5},
		// UUID-with-dashes uses only 17 distinct symbols (16 hex +
		// `-`); entropy ≈ log2(17) ≈ 4.09 ceiling. Real values fall
		// to ~3.4 because hex digits aren't perfectly uniform.
		{"uuid_v4", "550e8400-e29b-41d4-a716-446655440000", 3.0, 5.0},
		{"random_base64_32", "Xj9nQ8vL5pK2mZ7tR4wH6sB1aD3fG0iC", 4.5, 6.0},
		{"all_same", "aaaaaaaaaa", -0.01, 0.01},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shannonEntropy(tc.input)
			if got < tc.wantMin || got > tc.wantMax {
				t.Fatalf("entropy(%q) = %v, want in [%v,%v]", tc.input, got, tc.wantMin, tc.wantMax)
			}
		})
	}
}

func TestEntropy_Unicode(t *testing.T) {
	// Multi-byte runes must not skew the per-rune frequency count.
	got := shannonEntropy("éàü€😀")
	// 5 distinct runes equiprobable → entropy = log2(5) ≈ 2.32.
	if got < 2.2 || got > 2.4 {
		t.Fatalf("unicode entropy = %v, want ≈ 2.32", got)
	}
}
