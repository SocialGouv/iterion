// Package backendcaps owns the declarative inventory of built-in backends.
// It is a leaf: the compiler and execution adapters read the same facts without
// importing each other. Implementation, published proof, and live target
// outcomes are separate observations; none implies either of the others.
package backendcaps

import (
	_ "embed"
	"encoding/json"
	"slices"
)

// Built-in registration names. Custom registries remain open to other names.
const (
	ClaudeCode = "claude_code"
	Claw       = "claw"
	Codex      = "codex"
	Grok       = "grok"
	Kimi       = "kimi"
	OpenCode   = "opencode"
	Pi         = "pi"
)

// Capability keys name the precise predicate consumed by an adapter. In
// particular, receiving a tool list is not resolving it, and consuming a
// SessionID is not packing a persistent conversation.
const (
	PermissionAsk          = "permission-ask"
	PermissionDeny         = "permission-deny"
	PermissionExternalHook = "permission-external-hook"
	ReasoningEffort        = "reasoning-effort"
	CommandOverride        = "command-override"
	ProviderChain          = "provider-chain"
	InteractionAsync       = "interaction-async"
	InteractionSync        = "interaction-sync"
	ToolList               = "tool-list"
	ToolResolution         = "tool-resolution"
	SessionIDResume        = "session-id-resume"
	SessionPersist         = "session-persist"
	MCPForwarding          = "mcp-forwarding"
	AutoMemory             = "auto-memory"
	AutoMemoryPrompt       = "auto-memory-prompt"

	// ImagePresence is CLI reachability in the images iterion publishes: does
	// a published image put this backend's executable on PATH? It is derived
	// from the image manifests, not from prose — see
	// TestImagePresenceIsDerivedFromTheImageManifests. It says nothing about
	// multimodal image INPUT, which is a separate axis.
	//
	// Partial here means reachable in ONE published family and not the other
	// (codex: on the runner images' PATH, absent from the stock sandbox), so a
	// caller that needs a per-image answer must not read this cell as a
	// boolean — Offers reports partial as offered.
	ImagePresence = "image-presence"
)

// Published image families scope the image-presence cell. CLI reachability is
// one fact per family and two different facts across them — the runner images
// (the application image and the cloud runner) carry the pinned CLIs' bin
// directory on PATH, the stock sandbox images extend no PATH and reach only
// their explicit symlinks — so the question "may this image run that backend"
// is only answerable per family. A scope is not a backend-wide verdict: see
// [ScopedVerdict].
const (
	ImageFamilyRunner  = "runner"
	ImageFamilySandbox = "sandbox"
)

// Implementation verdicts describe existing wiring, not live maturity.
const (
	Shipped      = "shipped"
	Partial      = "partial"
	TypedRefused = "typed-refused"
	Inert        = "inert"
	Unknown      = "unknown"

	// UnwiredNoImage is the published vocabulary for "the capability has no
	// wiring because no image carries the CLI it needs". The gap is the
	// image's contents, not the backend: an operator image that ships the CLI
	// lifts it (#1643).
	UnwiredNoImage = "unwired (no image)"
)

// Cell records one scoped fact. Proof cells retain the published vocabulary
// verbatim, including compound verdicts; Evidence names repository lines or
// tests. An unknown is explicit, never an empty cell.
type Cell struct {
	Name     string   `json:"name"`
	Axis     string   `json:"axis"`
	Verdict  string   `json:"verdict"`
	Evidence []string `json:"evidence"`
	Detail   string   `json:"detail,omitempty"`

	// Scoped carries the per-scope verdicts of a cell whose single verdict
	// cannot answer a scoped question. image-presence is the case: codex is
	// on the runner images' PATH and absent from the stock sandbox, which
	// merges to Partial — a caller admitting ONE image needs the side, and
	// deriving it from the merge would be a guess. Keys are the scopes the
	// axis defines (the ImageFamily* names here). Empty on every cell whose
	// axis has no scopes.
	Scoped map[string]string `json:"scoped,omitempty"`

	// Subject names what the verdict is ABOUT when the capability is not the
	// backend itself: the image-presence cells name the executable an image
	// must put on PATH, which is what a refusal has to print. It is the
	// adapter's own DefaultBinary, held to it by
	// TestImagePresenceIsDerivedFromTheImageManifests.
	Subject string `json:"subject,omitempty"`
}

// Backend is one declarative row. Zero ranks mean absent from that selection
// surface, not absent from the registry. NativeTools is the existing exclusion
// roster, not a promise to enumerate everything an upstream CLI can execute.
type Backend struct {
	Name              string   `json:"name"`
	DefaultPreference int      `json:"default_preference"`
	DetectionOrder    int      `json:"detection_order"`
	NativeTools       []string `json:"native_tools"`
	Cells             []Cell   `json:"cells"`
	Proof             []Cell   `json:"proof"`
	SystemPrompt      string   `json:"system_prompt"`
}

// Inventory keeps the seed provenance reviewable. SourceCommit is the original
// observation, not an assertion that every later cell was measured there.
type Inventory struct {
	Schema       int       `json:"schema"`
	SourceCommit string    `json:"source_commit"`
	Epic         int       `json:"epic"`
	Backends     []Backend `json:"backends"`
}

//go:embed registry.json
var registryJSON []byte

//go:embed live_seed.json
var liveSeedJSON []byte

var inventory = decodeInventory()

func decodeInventory() Inventory {
	var v Inventory
	if err := json.Unmarshal(registryJSON, &v); err != nil {
		panic(err) // A malformed compiled-in declaration is a build defect.
	}
	return v
}

// Snapshot returns an independent copy, including nested slices.
func Snapshot() Inventory { return decodeInventory() }

// LiveSeed returns the complete target-level ledger observation. The ledger
// has no backend dimension: assigning its passes to a backend would invent
// evidence. Callers receive a copy of the JSON, including run metadata.
func LiveSeed() json.RawMessage { return slices.Clone(liveSeedJSON) }

// Names returns all built-in names in stable order.
func Names() []string {
	names := make([]string, 0, len(inventory.Backends))
	for _, b := range inventory.Backends {
		names = append(names, b.Name)
	}
	return names
}

// SystemPrompt reports the existing composition mode; unknown/custom backends
// retain the standalone default. It does not normalize registration names.
func SystemPrompt(backend string) string {
	for _, b := range inventory.Backends {
		if b.Name == backend {
			return b.SystemPrompt
		}
	}
	return "standalone"
}

// Verdict returns the implementation verdict, or unknown for an unregistered
// name/key. Lookup is exact: callers retain their own normalization policy.
func Verdict(backend, capability string) string {
	for _, b := range inventory.Backends {
		if b.Name == backend {
			for _, c := range b.Cells {
				if c.Name == capability {
					return c.Verdict
				}
			}
		}
	}
	return Unknown
}

// cellOf returns the implementation cell of an exact (backend, capability)
// pair. Lookup is exact, like [Verdict]'s: a caller's normalization policy is
// its own.
//
// It hands back the REGISTRY's own Cell, whose Evidence slice and Scoped map
// are the compiled-in ones: read them here and return values, never the
// containers. [Snapshot] is the copying reader, and
// TestTheScopedCellsCannotBeMutatedThroughAProjection pins the difference.
func cellOf(backend, capability string) (Cell, bool) {
	for _, b := range inventory.Backends {
		if b.Name != backend {
			continue
		}
		for _, c := range b.Cells {
			if c.Name == capability {
				return c, true
			}
		}
	}
	return Cell{}, false
}

// ScopedVerdict returns the verdict a cell records for ONE scope of its axis,
// or Unknown for an unregistered backend, capability or scope. A cell that
// declares no scopes answers Unknown for every scope: a merged verdict is not
// a scoped one, and reading Partial as "absent here" (or as "present here") is
// how an unmeasured side becomes a false refusal.
func ScopedVerdict(backend, capability, scope string) string {
	c, ok := cellOf(backend, capability)
	if !ok {
		return Unknown
	}
	if v, ok := c.Scoped[scope]; ok {
		return v
	}
	return Unknown
}

// UnservedByPublishedImages reports that NO image iterion publishes carries
// the backend's CLI — the image-presence cell itself says UnwiredNoImage. It
// is the fact a reader who does not know which image a run will use can state:
// the compiler's C321 and the launch-time warning read it, and neither
// refuses, because the operator's own image may carry the CLI (#1643).
func UnservedByPublishedImages(backend string) bool {
	return Verdict(backend, ImagePresence) == UnwiredNoImage
}

// UnservedByImageFamily reports the MEASURED absence of a backend's CLI from
// the published images of ONE family: the image-presence cell says
// UnwiredNoImage for that family, explicitly. It is what a reader holding a
// RESOLVED image asks — codex is served on the runner side and unserved in the
// stock sandbox, a split [UnservedByPublishedImages] cannot express.
//
// Neither is the negation of [Offers]. An unregistered name — a custom backend
// an operator registered, whose image is equally theirs — and a family this
// build has no scope for both answer false, because refusing a name on silence
// refuses what nobody measured. The gap they do report is the image's
// contents, so a refusal must say which image and leave the operator the way
// out.
func UnservedByImageFamily(backend, family string) bool {
	return ScopedVerdict(backend, ImagePresence, family) == UnwiredNoImage
}

// ServedByImage lists the backends whose CLI the published images of one
// family DO put on PATH, in registry order. It is what a refusal's remedy
// names — "route to one of these" — read from the same cells as the refusal
// instead of a prose list that drifts the day an image bakes one more.
func ServedByImage(family string) []string {
	var names []string
	for _, b := range inventory.Backends {
		if ScopedVerdict(b.Name, ImagePresence, family) == Shipped {
			names = append(names, b.Name)
		}
	}
	return names
}

// ImageCLI returns the executable a published image must put on PATH for the
// backend — the subject of its image-presence cell — or "" when the backend is
// unregistered or its cell names none.
func ImageCLI(backend string) string {
	c, _ := cellOf(backend, ImagePresence)
	return c.Subject
}

// Offers reports existing wiring, including conditional wiring. A partial
// capability still requires the adapter's runtime transport/context checks.
// Never use its negation to reject custom backends; test an explicit refusal.
func Offers(backend, capability string) bool {
	v := Verdict(backend, capability)
	return v == Shipped || v == Partial
}

// Set projects a fresh membership map for an existing enumerating consumer.
// No verdicts selects shipped/partial; explicit verdicts select those only.
func Set(capability string, verdicts ...string) map[string]bool {
	if len(verdicts) == 0 {
		verdicts = []string{Shipped, Partial}
	}
	set := make(map[string]bool)
	for _, b := range inventory.Backends {
		if slices.Contains(verdicts, Verdict(b.Name, capability)) {
			set[b.Name] = true
		}
	}
	return set
}

// PermissionModes projects the existing compiler gate table.
func PermissionModes() map[string]map[string]bool {
	modes := make(map[string]map[string]bool)
	for _, b := range inventory.Backends {
		for _, mode := range []string{"ask", "deny"} {
			if Offers(b.Name, "permission-"+mode) {
				if modes[b.Name] == nil {
					modes[b.Name] = make(map[string]bool)
				}
				modes[b.Name][mode] = true
			}
		}
	}
	return modes
}

// NativeTools returns a copy of the seeded exclusion roster in legacy order.
func NativeTools(backend string) []string {
	for _, b := range inventory.Backends {
		if b.Name == backend {
			return slices.Clone(b.NativeTools)
		}
	}
	return nil
}

// DefaultPreference preserves the opt-in-only status of the other backends.
func DefaultPreference() []string {
	return rankedNames(func(b Backend) int { return b.DefaultPreference })
}

// DetectionNames preserves probe membership and presentation order. A backend
// absent here still exists; wiring a new probe is a separate verdict change.
func DetectionNames() []string { return rankedNames(func(b Backend) int { return b.DetectionOrder }) }

func rankedNames(rank func(Backend) int) []string {
	rows := slices.Clone(inventory.Backends)
	slices.SortFunc(rows, func(a, b Backend) int { return rank(a) - rank(b) })
	var names []string
	for _, b := range rows {
		if rank(b) > 0 {
			names = append(names, b.Name)
		}
	}
	return names
}
