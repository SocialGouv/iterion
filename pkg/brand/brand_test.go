package brand

import (
	"bytes"
	"image/png"
	"testing"
)

// The embedded files are generated; these pin what every consumer relies on,
// so a regenerated master that breaks an upload fails here, not on a forge.
func TestBotAvatar_DecodesSquare(t *testing.T) {
	for _, v := range []Variant{VariantPlain, VariantCircle} {
		data := BotAvatar(v)
		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: not a PNG: %v", v, err)
		}
		if cfg.Width != cfg.Height || cfg.Width < 256 {
			t.Errorf("%s: want a square of at least 256 px, got %dx%d", v, cfg.Width, cfg.Height)
		}
	}
}

// GitLab refuses anything above 200 KiB on PUT /user/avatar; the plain variant
// is what that endpoint receives.
func TestBotAvatar_PlainFitsGitLabLimit(t *testing.T) {
	if n := len(BotAvatar(VariantPlain)); n > GitLabAvatarMaxBytes {
		t.Fatalf("plain avatar is %d bytes, above GitLab's %d-byte limit — regenerate with a stronger quantisation", n, GitLabAvatarMaxBytes)
	}
}

func TestBotAvatar_ReturnsACopy(t *testing.T) {
	a := BotAvatar(VariantPlain)
	a[0] = 0
	if b := BotAvatar(VariantPlain); b[0] == 0 {
		t.Fatal("BotAvatar handed out the embedded slice itself")
	}
}

func TestParseVariant(t *testing.T) {
	for in, want := range map[string]Variant{"": VariantPlain, "plain": VariantPlain, " Circle ": VariantCircle} {
		got, err := ParseVariant(in)
		if err != nil || got != want {
			t.Errorf("ParseVariant(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseVariant("square"); err == nil {
		t.Error("ParseVariant(square) accepted an unknown variant")
	}
	for _, v := range []Variant{VariantPlain, VariantCircle} {
		if got, ok := VariantForFilename(v.Filename()); !ok || got != v {
			t.Errorf("VariantForFilename(%s) = %q, %v", v.Filename(), got, ok)
		}
	}
	if _, ok := VariantForFilename("../etc/passwd"); ok {
		t.Error("VariantForFilename accepted an unknown file name")
	}
}
