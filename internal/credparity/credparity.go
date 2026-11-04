// Package credparity is ADR-161's parity table: one row per shape of "which
// credential does this call spend?", and the one answer every reader of that
// question owes it — the dispatch (the client it builds, the env it crosses),
// the launch's admission, the usage-cap pre-flight's meter, the spend ledger
// and the credential plan's fine table.
//
// The table is data, read by a harness in each reader's package (each reader
// is unexported where it lives): pkg/backend/model drives the dispatch,
// pkg/runtime the admission, pkg/runner the meter, the ledger and the plan.
// Each harness asserts its reader's answer equals the row's Want, so the
// readers agree with each other through the row.
//
// A row a reader still disagrees with on main carries a Pending entry naming
// the reader, the ADR-161 unit whose landing makes it agree, and the answer it
// gives today. The harness asserts that answer — the divergence stays pinned,
// never a red test — and asserts it differs from Want, so the unit that fixes
// the reader turns the row red until it deletes the entry: a row cannot keep
// a stale Pending.
package credparity

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// Reader names one reader of a call's credential.
type Reader string

const (
	// Dispatch is the credential the built client carries, in process, or
	// the one the container's own resolution reads off the crossed env.
	Dispatch Reader = "dispatch"
	// Admission is the launch's verdict: the start proceeds, or is refused
	// with a typed code.
	Admission Reader = "admission"
	// Meter is the usage-cap pre-flight's key for the node's route.
	Meter Reader = "meter"
	// Ledger is the run slot the spend ledger books the call's spend on.
	Ledger Reader = "ledger"
	// Plan is the fingerprint the fine table books the call's spend on,
	// joined through the publisher's credential plan.
	Plan Reader = "plan"
)

// Readers lists every reader, in the order a report names them.
var Readers = []Reader{Dispatch, Admission, Meter, Ledger, Plan}

// Channel is where the spent credential comes from — ADR-161 decision 3's
// vocabulary.
type Channel string

const (
	RunKey      Channel = "run key"
	PinnedKey   Channel = "pinned key"
	RunForfait  Channel = "run forfait"
	GatewayPair Channel = "gateway pair"
	PodEnv      Channel = "pod env"
	HostLogin   Channel = "host login"
	OwnConfig   Channel = "own config"
)

// Reason is why a call spends nothing — ADR-161 decision 7's vocabulary.
type Reason string

const (
	AmbientStamp       Reason = "ambient-stamp"
	SubscriptionSwitch Reason = "subscription-switch"
	HintUnfunded       Reason = "hint-unfunded"
	NoCredential       Reason = "no-credential"
	OutsideAllowlist   Reason = "outside-allowlist"
	ContentBoundary    Reason = "content-boundary"
	NoArm              Reason = "no-arm"
	ArmPending         Reason = "arm-pending"
)

// The admission codes ADR-161 decision 7 assigns. Only the first exists on
// main; the other two are the ones U1's single admission site emits.
const (
	CodeUnreachable           = "SANDBOX_CREDENTIAL_UNREACHABLE"
	CodeAmbientRefused        = "CREDENTIAL_AMBIENT_REFUSED"
	CodeSubscriptionForbidden = "CREDENTIAL_SUBSCRIPTION_FORBIDDEN"
)

// AdmissionCode is the code a launch refused for reason carries.
func AdmissionCode(reason Reason) string {
	switch reason {
	case AmbientStamp, ArmPending:
		return CodeAmbientRefused
	case SubscriptionSwitch:
		return CodeSubscriptionForbidden
	}
	return CodeUnreachable
}

// Spend is one reader's answer: the leg that spends, its channel, the run slot
// it books on ("" for none of the run's) and the credential value the request
// carries — or, Refusal set, the reason nothing spends.
type Spend struct {
	Leg        int
	Channel    Channel
	Slot       string
	Credential string
	Refusal    Reason
}

// Refused reports that nothing spends.
func (s Spend) Refused() bool { return s.Refusal != "" }

// Spent is a spend on leg of the node's chain.
func Spent(leg int, ch Channel, slot, credential string) Spend {
	return Spend{Leg: leg, Channel: ch, Slot: slot, Credential: credential}
}

// Refuses is a call no leg funds.
func Refuses(r Reason) Spend { return Spend{Refusal: r} }

// Launches is an admission's answer that the start proceeds — admitted, or
// passing no admission at all.
var Launches = Spend{}

// Nobody is a ledger's or a plan's booking on none of the run's credentials.
var Nobody = Spend{}

// Trust is the run's trust.
type Trust string

const (
	// Fork is the untrusted fork lane (#1768): sandboxed, admission-judged.
	Fork Trust = "fork"
	// Trusted is an operator's run.
	Trusted Trust = "trusted"
)

// Role is who calls the model.
type Role string

const (
	NodeRole       Role = "node"
	SupervisorRole Role = "supervisor"
	AuxiliaryRole  Role = "auxiliary"
)

// The fixed values the rows spell their credentials with. Each is distinct,
// so a reader's answer names its channel by value alone.
const (
	ForfaitToken = "sk-ant-oat-run"
	RunAnthropic = "sk-run"
	RunZAI       = "zai-run"
	PodAnthropic = "sk-pod"
	PodBearer    = "tok-pod"
	PodZAI       = "zai-pod"
	HeaderBearer = "Bearer hdr-pod"
)

// Run is the run's sealed credentials and stamps.
type Run struct {
	// Keys are the run's default keys, by provider.
	Keys map[secrets.Provider]string
	// Forfait seals a Claude Code forfait whose access token is ForfaitToken.
	Forfait bool
	// Stamped is ADR-131 §7's stamp: this bundle's credentials ONLY.
	Stamped bool
	// Allowlist is the run's provider allowlist (ADR-129 §5); nil allows all.
	Allowlist []string
	// Plan stamps the publisher's credential plan on the run (#2288): its
	// slots were lent by a pool donor, and the fine table books on them.
	Plan bool
}

// Fingerprint is the audit fingerprint the rows give a run slot — one space
// for Credentials.Fingerprints and the bundle the plan reads.
func Fingerprint(slot string) string { return "fp-" + slot }

// Slots lists the run slots the bundle holds, sorted.
func (r Run) Slots() []string {
	var out []string
	for p := range r.Keys {
		out = append(out, string(p))
	}
	if r.Forfait {
		out = append(out, string(secrets.OAuthKindClaudeCode))
	}
	slices.Sort(out)
	return out
}

// Credentials is the run's ctx credentials, the forfait at forfaitDir (a
// directory WriteForfait filled; "" when the run holds none).
func (r Run) Credentials(forfaitDir string) secrets.Credentials {
	c := secrets.Credentials{
		AmbientRefused: r.Stamped,
		Fingerprints:   map[string]string{},
	}
	if len(r.Keys) > 0 {
		c.APIKeys = map[secrets.Provider]string{}
		for p, k := range r.Keys {
			c.APIKeys[p] = k
		}
	}
	if r.Forfait {
		c.OAuthCredentialFiles = map[string]string{string(secrets.OAuthKindClaudeCode): forfaitDir}
	}
	for _, slot := range r.Slots() {
		c.Fingerprints[slot] = Fingerprint(slot)
		if r.Plan {
			if c.PoolSourced == nil {
				c.PoolSourced = map[string]bool{}
			}
			c.PoolSourced[slot] = true
		}
	}
	return c
}

// Bundle is the sealed bundle the publisher derives the plan from.
func (r Run) Bundle() secrets.RunBundle {
	b := secrets.RunBundle{AmbientLLMRefused: r.Stamped}
	if len(r.Keys) > 0 {
		b.APIKeys = map[secrets.Provider]string{}
		b.APIKeyFingerprints = map[secrets.Provider]string{}
		for p, k := range r.Keys {
			b.APIKeys[p] = k
			b.APIKeyFingerprints[p] = Fingerprint(string(p))
		}
	}
	if r.Forfait {
		kind := string(secrets.OAuthKindClaudeCode)
		b.OAuthCredentials = map[string][]byte{kind: forfaitBlob()}
		b.OAuthFingerprints = map[string]string{kind: Fingerprint(kind)}
	}
	if r.Plan {
		b.PoolSourced = map[string]bool{}
		for _, slot := range r.Slots() {
			b.PoolSourced[slot] = true
		}
	}
	return b
}

func forfaitBlob() []byte {
	return []byte(`{"claudeAiOauth":{"accessToken":"` + ForfaitToken + `","expiresAt":4102444800000}}`)
}

// WriteForfait writes the run's Claude Code forfait into dir, the shape
// Credentials.OAuthDir("claude_code") names on a runner pod.
func WriteForfait(dir string) error {
	return os.WriteFile(filepath.Join(dir, ".credentials.json"), forfaitBlob(), 0o600)
}

// Leg is one element of the chain dispatch walks: its provider hint and the
// spec it sends ("" inherits the node's model).
type Leg struct {
	Provider string
	Model    string
}

// Node is the call's node: its backend, model and raw `provider:` field, and
// Legs — the chain the dispatch walks once the field is expanded, parsed and
// the allowlist applied. The dispatch harness checks Legs against the
// executor's own parse; the other harnesses read the spent leg from it.
type Node struct {
	ID       string
	Backend  string
	Model    string
	Provider string
	Legs     []Leg
}

// Spec is the model leg i sends.
func (n Node) Spec(i int) string {
	if i >= 0 && i < len(n.Legs) && n.Legs[i].Model != "" {
		return n.Legs[i].Model
	}
	return n.Model
}

// Hint is leg i's provider hint.
func (n Node) Hint(i int) string {
	if i >= 0 && i < len(n.Legs) {
		return n.Legs[i].Provider
	}
	return ""
}

// Pending pins a reader that disagrees with the row on main.
type Pending struct {
	Reader Reader
	// Unit is the ADR-161 unit whose landing makes the reader agree.
	Unit string
	// Today is the reader's answer on main. For Admission, Launches reads
	// "the start proceeds".
	Today Spend
	// Why names the divergence and the finding that saw it.
	Why string
}

// Row is one shape of the question and its one answer.
type Row struct {
	Name string
	// Origin is the bench witness, review finding or port the row comes from.
	Origin  string
	Posture map[string]string
	Run     Run
	Node    Node
	Role    Role
	// Sandboxed: the node runs in the run's sandbox; false runs it on the
	// host (`sandbox: none`, or an `auto` that degraded).
	Sandboxed bool
	Trust     Trust
	// Adopted: a subbot child that adopts its sandboxed parent's sandbox.
	Adopted bool
	Want    Spend
	Pending []Pending
}

// Judged reports whether ADR-161 has the launch judge the run: the fork lane
// and every stamped run (decision 4). A trusted, unstamped run's refusals
// still arrive at dispatch.
func (r Row) Judged() bool { return r.Trust == Fork || r.Run.Stamped }

// Expect is what reader must answer for the row: Want, or — while the reader
// still disagrees on main — the Pending entry pinning today's answer.
func (r Row) Expect(reader Reader) (Spend, *Pending) {
	for i := range r.Pending {
		if r.Pending[i].Reader == reader {
			return r.Want, &r.Pending[i]
		}
	}
	return r.Want, nil
}

// PostureNames are the environment names a harness blanks before it applies
// a row's posture, so a row's pod is the row's and not the host's.
var PostureNames = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS",
	"ANTHROPIC_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "ZAI_API_KEY",
	"ITERION_FORBID_SUBSCRIPTION_OAUTH", "CLAW_PROVIDER",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
	"OPENAI_API_KEY", "OPENAI_BASE_URL", "ITERION_OPENAI_USE_OAUTH", "CODEX_HOME",
	"MOONSHOT_API_KEY", "MOONSHOT_BASE_URL", "XAI_API_KEY", "AZURE_OPENAI_API_KEY",
	"GEMINI_API_KEY", "GOOGLE_API_KEY", "GROQ_API_KEY", "DEEPSEEK_API_KEY", "MISTRAL_API_KEY",
	"OPENROUTER_API_KEY", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
	"OPENAI_COMPATIBLE_BASE_URL", "OPENAI_COMPATIBLE_API_KEY", "ITERION_LLM_ENDPOINT_ALLOW_PRIVATE",
	"ITERION_PI_AGENT_DIR", "PI_CODING_AGENT_DIR", "CLAW_USER_AGENT", "ITERION_LLM_USER_AGENT",
}

// Label is a stable subtest name for the row.
func (r Row) Label() string {
	return strings.NewReplacer(" ", "_", "/", "_").Replace(r.Name)
}
