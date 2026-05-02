package store

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

// runNameAdjectives is the curated, sober adjective pool used to derive
// human-friendly run names. Order is part of the contract: changing it
// would invalidate the seed → name mapping for all existing runs whose
// names were derived from this list.
var runNameAdjectives = []string{
	"calm", "clear", "cool", "crisp", "deep", "dim", "distant", "dry", "dusky", "early",
	"even", "faint", "far", "fine", "fleet", "frank", "high", "keen", "late", "lean",
	"level", "lithe", "lone", "low", "lucid", "mild", "mute", "near", "neat", "pale",
	"plain", "prime", "quiet", "raw", "sharp", "slow", "soft", "sober", "spare", "stark",
	"steady", "still", "swift", "tacit", "terse", "vast", "vivid", "warm", "wide", "wild",
	"white", "ashen", "amber", "smooth", "rough", "brisk", "blunt", "bright", "bold", "brave",
	"gentle", "agile", "grave", "hardy", "idle", "lush", "mellow", "modest", "noble", "patient",
	"quick", "ready", "rich", "royal", "sleek", "stout", "sturdy", "suave", "stoic", "trim",
}

// runNameNouns is the curated noun pool — trees, stones, water, terrain,
// cosmos, fauna — selected for sobriety and tech-friendly readability.
// Order is part of the contract (see runNameAdjectives).
var runNameNouns = []string{
	"cedar", "oak", "elm", "pine", "birch", "ash", "willow", "beech", "juniper", "larch",
	"maple", "alder", "poplar", "quartz", "slate", "basalt", "agate", "onyx", "jade", "flint",
	"opal", "marble", "granite", "mica", "chert", "coral", "pearl", "copper", "iron", "tin",
	"bronze", "steel", "glass", "salt", "river", "brook", "fjord", "delta", "tide", "basin",
	"isle", "creek", "harbor", "marsh", "pond", "reef", "gulf", "bay", "cove", "ridge",
	"vale", "mesa", "glade", "meadow", "dune", "hill", "peak", "hollow", "orion", "lyra",
	"vega", "atlas", "nova", "polaris", "sirius", "comet", "prism", "ember", "pyre", "arc",
	"helix", "vertex", "pivot", "hawk", "heron", "falcon", "lynx", "otter", "raven", "anchor",
}

// GenerateRunName derives a stable, human-friendly run label from an
// opaque seed. Same seed → same name. The output is kebab-case ASCII,
// path- and URL-safe: <adjective>-<noun>-<4hex>, e.g. "swift-cedar-a3f2".
//
// The 4-char hex suffix (16 bits) lifts the namespace from ~6.4k
// adj-noun pairs to ~419M combinations, keeping pairwise collision
// risk negligible at any realistic run count.
func GenerateRunName(seed string) string {
	h := sha256.Sum256([]byte(seed))
	adj := runNameAdjectives[binary.BigEndian.Uint16(h[0:2])%uint16(len(runNameAdjectives))]
	noun := runNameNouns[binary.BigEndian.Uint16(h[2:4])%uint16(len(runNameNouns))]
	suffix := hex.EncodeToString(h[4:6])
	return adj + "-" + noun + "-" + suffix
}
