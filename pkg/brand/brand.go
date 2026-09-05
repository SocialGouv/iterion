// Package brand embeds iterion's bot identity — the mascot avatar of the
// official `iterion-bot` GitHub account — so the server can upload it onto the
// accounts it operates (forge.AvatarSetter) and serve it for the uploads it
// cannot do itself (a GitHub App's logo has no API). The PNGs here are
// GENERATED from the masters under assets/brand/ by `task brand:gen`; edit the
// masters, never these copies — `task brand:check` fails on a stale one.
package brand

import (
	"bytes"
	_ "embed"
	"fmt"
	"strings"
)

// Variant selects which rendering of the mascot a caller wants.
type Variant string

const (
	// VariantPlain is the account avatar: the mascot on a transparent
	// background, pixel-identical to what the iterion-bot account wears. It is
	// what goes onto a forge bot account, so every identity shows one face.
	VariantPlain Variant = "plain"
	// VariantCircle is the badge form — dark disc + ring — for surfaces that
	// want a self-contained mark whatever sits behind it: a GitHub App logo,
	// the docs, the favicons.
	VariantCircle Variant = "circle"
)

//go:embed iterion-bot.png
var plainPNG []byte

//go:embed iterion-bot-circle.png
var circlePNG []byte

// GitLabAvatarMaxBytes is GitLab's hard limit on `PUT /user/avatar` (200 KiB).
// Either variant can be asked for on that endpoint, so both must stay under
// it; brand_test.go pins the invariant against a regenerated master.
const GitLabAvatarMaxBytes = 200 * 1024

// ParseVariant reads an operator-supplied variant name; "" is the plain one.
func ParseVariant(s string) (Variant, error) {
	switch Variant(strings.ToLower(strings.TrimSpace(s))) {
	case "", VariantPlain:
		return VariantPlain, nil
	case VariantCircle:
		return VariantCircle, nil
	}
	return "", fmt.Errorf("brand: unknown avatar variant %q (want plain or circle)", s)
}

// Filename is the file name a variant is served and uploaded under.
func (v Variant) Filename() string {
	if v == VariantCircle {
		return "iterion-bot-circle.png"
	}
	return "iterion-bot.png"
}

// VariantForFilename is Filename's inverse, for the /brand/{file} route.
func VariantForFilename(name string) (Variant, bool) {
	switch name {
	case VariantPlain.Filename():
		return VariantPlain, true
	case VariantCircle.Filename():
		return VariantCircle, true
	}
	return "", false
}

// BotAvatar returns a copy of the variant's PNG bytes (a copy, so a caller
// can never dent the embedded original).
func BotAvatar(v Variant) []byte {
	src := plainPNG
	if v == VariantCircle {
		src = circlePNG
	}
	return bytes.Clone(src)
}
