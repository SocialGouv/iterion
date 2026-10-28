package backendcaps

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update-backendcaps", false, "update the reviewed backend capability golden (never the HEAD migration oracle)")

func readJSON(t *testing.T, path string, target any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, target); err != nil {
		t.Fatal(err)
	}
}

// TestRegistryGolden is the future doctrine-review surface. Updating it is
// explicit and cannot regenerate the independent migration oracle below.
func TestRegistryGolden(t *testing.T) {
	v := struct {
		Registry Inventory       `json:"registry"`
		Live     json.RawMessage `json:"live"`
	}{Snapshot(), LiveSeed()}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, '\n')
	const path = "testdata/registry.golden.json"
	if *updateGolden {
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, want) {
		t.Fatal("registry/golden differs: review each cell and explicitly run -update-backendcaps; do not infer a proof upgrade from wiring")
	}
}

type headEnumerations struct {
	SourceCommit      string              `json:"source_commit"`
	Gates             map[string][]string `json:"gates"`
	Sets              map[string][]string `json:"sets"`
	NativeTools       []string            `json:"native_tools"`
	DefaultPreference []string            `json:"default_preference"`
	DetectionOrder    []string            `json:"detection_order"`
	HarnessOrder      []string            `json:"harness_order"`
	SystemPrompt      map[string]string   `json:"system_prompt"`
}

// TestSeedZeroMove compares with literals extracted from pre-migration HEAD,
// not with expectations generated from registry.json. Changes to this oracle
// are explicit verdict changes too, and require the same review as the golden.
//
// One row has moved since the migration, and it is the only one: `native_tools`
// for claude_code, from the fourteen names pre-migration HEAD carried to twenty
// (#1651). The six added — Agent, AskUserQuestion, BashOutput, KillShell,
// ScheduleWakeup, SlashCommand — were tools the roster had drifted away from
// while iterion rendered headers for them, which made each one a tool no
// `tools:` list could remove. The oracle was edited by hand for it, because
// there is deliberately no flag that regenerates it: its diff is the review.
// Every other row still reads pre-migration HEAD verbatim.
func TestSeedZeroMove(t *testing.T) {
	var seed headEnumerations
	readJSON(t, "testdata/head-enumerations.json", &seed)
	if Snapshot().SourceCommit != seed.SourceCommit {
		t.Fatal("seed provenance differs")
	}
	for name, want := range seed.SystemPrompt {
		if got := SystemPrompt(name); got != want {
			t.Errorf("%s system prompt moved: got %s, HEAD %s", name, got, want)
		}
	}
	gotGates := map[string][]string{}
	for b, modes := range PermissionModes() {
		for mode := range modes {
			gotGates[b] = append(gotGates[b], mode)
		}
		slices.Sort(gotGates[b])
	}
	if !reflect.DeepEqual(gotGates, seed.Gates) {
		t.Errorf("permission verdict moved: got %v, HEAD %v", gotGates, seed.Gates)
	}
	projections := map[string]map[string]bool{
		"reasoning-effort":         Set(ReasoningEffort),
		"permission-external-hook": Set(PermissionExternalHook),
		"command-ignored":          Set(CommandOverride, Inert),
		"provider-chain-ignored":   Set(ProviderChain, Inert),
		"session-id-resume":        Set(SessionIDResume),
		"session-persist":          Set(SessionPersist),
		"mcp-forwarding":           Set(MCPForwarding),
		"async-refused":            Set(InteractionAsync, TypedRefused),
		"sync-inert":               Set(InteractionSync, Inert),
		"tool-list":                Set(ToolList),
		"tool-resolution":          Set(ToolResolution),
		"auto-memory":              Set(AutoMemory),
		"auto-memory-prompt":       Set(AutoMemoryPrompt),
	}
	if len(seed.Sets) != len(projections) {
		t.Fatal("a HEAD enumeration has no witness")
	}
	for name, set := range projections {
		var got []string
		for b := range set {
			got = append(got, b)
		}
		slices.Sort(got)
		if !slices.Equal(got, seed.Sets[name]) {
			t.Errorf("%s verdict moved: got %v, HEAD %v", name, got, seed.Sets[name])
		}
	}
	for name, pair := range map[string][2][]string{
		"native roster": {NativeTools(ClaudeCode), seed.NativeTools},
		"preference":    {DefaultPreference(), seed.DefaultPreference},
		"detection":     {DetectionNames(), seed.DetectionOrder},
		"harnesses":     {Names(), seed.HarnessOrder},
	} {
		if !slices.Equal(pair[0], pair[1]) {
			t.Errorf("%s moved: got %v, HEAD %v", name, pair[0], pair[1])
		}
	}
}

// The complete ledger comparison catches target swaps with unchanged totals,
// run-date/cost/ref loss, and forgotten new targets. It neither runs a model
// nor assigns a target-level pass to a backend.
func TestLiveLedgerZeroMove(t *testing.T) {
	var source, seed any
	readJSON(t, "../../../e2e/testdata/live/ledger.json", &source)
	if err := json.Unmarshal(LiveSeed(), &seed); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(source, seed) {
		t.Fatal("live seed differs cell-for-cell from the committed ledger; explicitly reconcile the observation and golden")
	}
}

func TestPublishedProofZeroMove(t *testing.T) {
	// Column identity is part of the observation: two columns with the same
	// verdict must not be exchangeable. This oracle is independent of the seed.
	columns := [][2]string{
		{"structured-output", "structured-output"},
		{"permission-ask", "permission"},
		{"session-resume-fork", "session"},
		{"tool-events-cost", "metering"},
		{"template-context", "templates"},
		{"sandbox", "sandbox"},
		{"mcp", "MCP"},
		{"ask-user", "interaction"},
		{"workspace-commands", "workspace-commands"},
	}
	data, err := os.ReadFile("../../../docs/backends.md")
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string][]string{}
	for line := range strings.SplitSeq(string(data), "\n") {
		parts := strings.Split(strings.Trim(line, "|"), "|")
		if len(parts) != 10 {
			continue
		}
		name := strings.Trim(strings.TrimSpace(parts[0]), "`")
		if !slices.Contains(Names(), name) {
			continue
		}
		for _, part := range parts[1:] {
			rows[name] = append(rows[name], strings.TrimSpace(part))
		}
	}
	for _, b := range Snapshot().Backends {
		if len(b.Proof) != len(columns) {
			t.Errorf("%s proof column count moved", b.Name)
			continue
		}
		var got []string
		for i, c := range b.Proof {
			if c.Name != columns[i][0] || c.Axis != columns[i][1] {
				t.Errorf("%s proof column identity moved at %d: %s/%s", b.Name, i, c.Name, c.Axis)
			}
			got = append(got, c.Verdict)
		}
		if !slices.Equal(got, rows[b.Name]) {
			t.Errorf("%s published proof moved: registry %v, docs %v", b.Name, got, rows[b.Name])
		}
	}
}

func TestInventoryIntegrity(t *testing.T) {
	names := map[string]bool{}
	ranks := [2]map[int]bool{{}, {}}
	var keys []string
	for _, b := range Snapshot().Backends {
		if b.Name == "" || names[b.Name] {
			t.Fatalf("duplicate/empty backend %q", b.Name)
		}
		names[b.Name] = true
		for i, rank := range []int{b.DefaultPreference, b.DetectionOrder} {
			if rank < 0 || rank > 0 && ranks[i][rank] {
				t.Errorf("%s invalid/duplicate rank %d", b.Name, rank)
			}
			ranks[i][rank] = true
		}
		var gotKeys []string
		for group, cells := range map[string][]Cell{"implementation": b.Cells, "proof": b.Proof} {
			seen := map[string]bool{}
			for _, c := range cells {
				if c.Name == "" || c.Axis == "" || c.Verdict == "" || len(c.Evidence) == 0 || seen[c.Name] {
					t.Errorf("%s/%s invalid cell %+v", b.Name, group, c)
				}
				seen[c.Name] = true
				if group == "implementation" {
					if !slices.Contains([]string{Shipped, Partial, TypedRefused, Inert, Unknown, "unwired (gap)", "unwired (no image)"}, c.Verdict) {
						t.Errorf("%s/%s invalid implementation verdict %q", b.Name, c.Name, c.Verdict)
					}
					gotKeys = append(gotKeys, c.Name)
				}
				for _, ref := range c.Evidence {
					path, anchor, ok := strings.Cut(ref, ":")
					if !ok || filepath.IsAbs(path) || strings.Contains(path, "..") {
						t.Errorf("invalid repository evidence %q", ref)
						continue
					}
					data, err := os.ReadFile(filepath.Join("../../..", path))
					if err != nil {
						t.Error(err)
						continue
					}
					if line, err := strconv.Atoi(anchor); err == nil {
						if line < 1 || line > bytes.Count(data, []byte{'\n'})+1 {
							t.Errorf("evidence line outside file: %s", ref)
						}
					} else if !strings.HasPrefix(anchor, "Test") || !bytes.Contains(data, []byte("func "+anchor+"(")) {
						t.Errorf("missing evidence test: %s", ref)
					}
				}
			}
		}
		slices.Sort(gotKeys)
		if keys == nil {
			keys = gotKeys
		} else if !slices.Equal(keys, gotKeys) {
			t.Errorf("%s omits a capability cell", b.Name)
		}
	}
}

func TestCopiesAndUnknowns(t *testing.T) {
	nativeBefore := slices.Clone(NativeTools(ClaudeCode))
	for _, name := range []string{"", "auto", "custom", "CLAUDE_CODE", " claude_code "} {
		if Offers(name, PermissionAsk) || Verdict(name, InteractionAsync) == TypedRefused {
			t.Errorf("unknown name %q acquires a capability/refusal", name)
		}
	}
	before, err := json.Marshal(Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	copy := Snapshot()
	copy.Backends[0].Cells[0].Evidence[0] = "mutated"
	copy.Backends[0].Cells[0].Verdict = "mutated"
	copy.Backends[0].NativeTools[0] = "mutated"
	native := NativeTools(ClaudeCode)
	native[0] = "mutated"
	set := Set(PermissionAsk)
	delete(set, ClaudeCode)
	modes := PermissionModes()
	delete(modes[ClaudeCode], "ask")
	live := LiveSeed()
	live[0] = '!'
	after, err := json.Marshal(Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !Offers(ClaudeCode, PermissionAsk) || LiveSeed()[0] != '{' {
		t.Fatal("a caller mutates the registry through a projection")
	}
	if !slices.Equal(nativeBefore, NativeTools(ClaudeCode)) {
		t.Fatal("a caller mutates the native roster through its projection")
	}
}
