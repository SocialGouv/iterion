package detector

import "testing"

func TestRule_LuhnPostFilter(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"4532015112830366", true},  // valid Visa
		{"4532015112830367", false}, // invalid (last digit + 1)
		{"4111111111111111", true},  // standard Visa test number
		{"0000000000000000", false}, // all-zero rejected by isAllSameDigit
		{"1111111111111111", false}, // all-same rejected
		{"1234567890123456", false}, // not Luhn
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := validateLuhn(tc.input)
			if got != tc.want {
				t.Fatalf("validateLuhn(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestRule_Mod97PostFilter(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"FR1420041010050500013M02606", true},
		{"FR1420041010050500013M02607", false},
		{"DE89370400440532013000", true},
		{"GB29NWBK60161331926819", true},
		{"AB12", false}, // too short
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := validateIBANMod97(tc.input)
			if got != tc.want {
				t.Fatalf("validateIBANMod97(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestRule_LooksLikeIdentifier(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"NormalLookingName", true},     // no digit, no special
		{"HasDigit1", false},            // has digit
		{"snake_case", false},           // has special
		{"with-hyphen", false},          // has special
		{"abc123def", false},            // has digit
		{"", true},                      // empty → identifier-ish
		{"OnlyAlphabeticSegment", true}, // textbook identifier
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := looksLikeIdentifier(tc.input)
			if got != tc.want {
				t.Fatalf("looksLikeIdentifier(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestRule_AllowedCategories(t *testing.T) {
	if !allowedCategories("email", nil) {
		t.Fatal("nil filter should allow everything")
	}
	if !allowedCategories("email", []string{}) {
		t.Fatal("empty filter should allow everything")
	}
	if !allowedCategories("email", []string{"email", "url"}) {
		t.Fatal("matching filter should allow")
	}
	if allowedCategories("phone", []string{"email"}) {
		t.Fatal("non-matching filter should deny")
	}
}
