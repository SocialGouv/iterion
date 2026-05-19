package store

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"github.com/google/uuid"
)

// runNamePool1 is the atmospheric-qualifier slot: synth-tech, rock-grit
// and a sprinkle of mischief ("neon", "thunder", "savage", "feral",
// "turbo", "sassy", "wonky", "zesty", …) that set the mood. Order is
// part of the contract: changing it would invalidate the seed → name
// mapping for all subsequent runs derived from this list.
var runNamePool1 = []string{
	"neon", "prism", "chrome", "plasma", "quantum", "void", "fractal", "photon", "thunder", "hex",
	"pixel", "glitch", "vapor", "crystal", "neural", "sonic", "electric", "kinetic", "magnetic", "atomic",
	"cosmic", "lunar", "solar", "astral", "stellar", "orbital", "radiant", "savage", "cryo", "pyro",
	"holo", "turbo", "sassy", "wonky", "zesty", "helix", "vortex", "beam", "flare", "halo",
	"blaze", "shimmer", "mirage", "renegade", "feral", "phantom", "ghost", "opal", "obsidian", "onyx",
	"quartz", "ember", "ash", "dusk", "dawn", "twilight", "midnight", "aurora", "eclipse", "comet",
	"meteor", "nova", "polar", "arctic", "boreal", "synth", "retro", "riot", "fuzz", "blistered",
	"laser", "sonar", "radar", "wrecked", "scorching", "smoldering", "magneto", "blastoff", "molten", "gamma",
}

// runNamePool2 is the motion-or-state slot: kinetic verb-nouns mixing
// glide-and-drift ("ripple", "surge", …), stage-rock action ("shred",
// "thrash", "mosh", "riff", "howl", "growl", …), pure goof ("yeet",
// "bonk", "boop", "vroom", "fizz", "shimmy", …) and comic-book SFX
// ("wham", "whomp", "thwack", "sploosh", "splat"). All English —
// kept deliberately disjoint from the franglais verbs the studio
// ThinkingFooter rotates through during loading, so a run name is
// never mistaken for a transient spinner caption. Order is part of
// the contract (see runNamePool1).
var runNamePool2 = []string{
	"drift", "pulse", "ripple", "bloom", "surge", "echo", "wave", "shard", "haze", "flux",
	"smash", "slam", "shred", "crash", "thrash", "wail", "howl", "snarl", "roar", "growl",
	"blast", "kick", "riff", "strum", "yeet", "bonk", "boop", "thump", "mosh", "grind",
	"stomp", "vroom", "sparkle", "sizzle", "churn", "glide", "dash", "sweep", "swirl", "twist",
	"spin", "whirl", "dance", "dive", "plunge", "soar", "climb", "leap", "bound", "sprint",
	"hop", "bounce", "blink", "flash", "dart", "ray", "splat", "chase", "hunt", "scream",
	"burst", "wham", "flick", "gust", "whomp", "thwack", "sploosh", "jive", "jam", "throb",
	"pump", "swoop", "swerve", "prance", "shimmy", "shuffle", "jolt", "snap", "pop", "fizz",
}

// runNamePool3 is the coined-noun slot: portmanteaus and invented
// compounds blending synth-cosmos with garage-rock iconography and
// pure cartoon energy ("ampthunder", "riotchord", "overdrive",
// "feedbackwail", "voltageclash", "neondemon", "stellarcrash",
// "vortexyeet", "cometchonk", "polarpurr", "modupickle",
// "holohowdy", "vapordoodle", "plasmacackle", "bonkstorm",
// "riffboi", "fuzzgremlin", "snazzbomb", "kazoodriver",
// "vortexvape", "distortcat", "snazzraptor", "thunderchomp",
// "hyperdrift", "megamosh", "pizzazap", …). Like pool 2, the
// lexicon is deliberately disjoint from the studio ThinkingFooter
// franglais vocabulary — no `bidouille`/`magouille`/`ratiocine`/
// `schmilblick` derivatives — so run names can't be confused with
// loading captions. Order is part of the contract (see runNamePool1).
var runNamePool3 = []string{
	"foxhowl", "mothbeam", "owlspark", "hexglade", "pyrebloom", "lumicore", "starforge", "voiddriver", "glitchfox", "pixelriot",
	"neondrift", "prismfox", "chromethrash", "sonarsnoot", "beamspire", "ampthunder", "riotchord", "megamosh", "feedbackwail", "overdrive",
	"distortpedal", "riotpyre", "voltageclash", "retrosparkle", "neondemon", "jadeshred", "quartzhowl", "solarslam", "stellarcrash", "magnetoglyph",
	"novachime", "cryomantle", "auroraflux", "photonpunch", "laserdoom", "dawnglyph", "duskvane", "midnightkazoo", "vapordoodle", "plasmacackle",
	"etherspark", "vortexvape", "plasmasong", "quantumcurl", "pizzazap", "vectorvibe", "scalarsmol", "hyperdrift", "vortexyeet", "orbitcrest",
	"novazap", "cometchonk", "polarpurr", "arctickazoo", "borealroar", "synthsnarl", "cyberbloom", "modupickle", "fiberglyph", "onyxblaze",
	"ashglyph", "crystalbloom", "distortcat", "snazzraptor", "holohowdy", "prismpunk", "beamboi", "halospire", "voidvibe", "nebupunch",
	"lunaspire", "astralcrick", "kineticglide", "ionicpurr", "snazzbomb", "thunderchomp", "kazoodriver", "bonkstorm", "riffboi", "fuzzgremlin",
}

// GenerateRunName derives a stable, human-friendly run label from an
// opaque seed. Same seed → same name. The output is kebab-case ASCII,
// path- and URL-safe: <atmos>-<motion>-<coined>-<4hex>, e.g.
// "turbo-whomp-fuzzgremlin-a3f2".
//
// Three distinct word slots (atmospheric qualifier, motion/state,
// coined portmanteau) make parallel runs visually unmistakable, and
// the 4-char hex suffix (16 bits) keeps the combined namespace at
// 80×80×80×65 536 ≈ 34 billion — well beyond any realistic collision
// horizon.
func GenerateRunName(seed string) string {
	h := sha256.Sum256([]byte(seed))
	w1 := runNamePool1[binary.BigEndian.Uint16(h[0:2])%uint16(len(runNamePool1))]
	w2 := runNamePool2[binary.BigEndian.Uint16(h[2:4])%uint16(len(runNamePool2))]
	w3 := runNamePool3[binary.BigEndian.Uint16(h[4:6])%uint16(len(runNamePool3))]
	suffix := hex.EncodeToString(h[6:8])
	return w1 + "-" + w2 + "-" + w3 + "-" + suffix
}

// GenerateRunID returns a new UUIDv7 run identifier. Lexicographic
// order matches creation order.
//
// Returns an error when uuid.NewV7 fails (only happens under
// crypto/rand starvation, e.g. very early at boot or after entropy
// exhaustion). Callers are responsible for surfacing the failure to
// the operator rather than retrying — the zero UUID must never be
// substituted because concurrent callers would collide on it.
func GenerateRunID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("store: mint run id: %w", err)
	}
	return id.String(), nil
}
