package modelspecs

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The embedded snapshot is the offline fallback for deployments that never
// reach models.dev — the gateway catalog's exact lookups answer from it
// when the live table is not loaded. It is REFRESHED, never hand-edited:
// `task models:snapshot` (scripts/modelsnapshot) regenerates it, prints the
// changelog for the PR, and records the source digest so a reader can tell
// what it was built from.
//
// ITERION_MODEL_SPECS=off does not disable it: the snapshot is static,
// curated-by-tooling data with no network, the thing the switch exists to
// refuse.

//go:embed snapshot/models-dev.json
var snapshotBlob string

// SnapshotSchema is the snapshot file's format version. A reader that does
// not match refuses the file rather than guess. Schema 2 carries the three
// capability flags beside the numbers; the file is EMBEDDED, so the constant
// and the bytes it judges ship in the same binary — a mismatch is reachable
// only by hand-editing the snapshot, which is what the refusal names.
const SnapshotSchema = 2

// SnapshotHeader is the file's first line.
type SnapshotHeader struct {
	Schema       int    `json:"schema"`
	Source       string `json:"source"`
	SourceDigest string `json:"source_digest"` // sha256 of the raw models.dev body the entries were parsed from
	AsOf         string `json:"as_of"`         // RFC3339 UTC
}

// snapshotEntry is one line: the numeric fields the gateway catalog
// resolves, plus the three capability flags models.dev publishes. The flags
// are `*bool,omitempty` for the same reason Spec's are — a flag the source
// did not state is ABSENT from the line and reads unknown, while a stated
// false crosses as false. The distinction is the point: `tool_call:false` is
// what turns a reader's warning into a refusal, and an embedding model
// offline must refuse, not warn.
type snapshotEntry struct {
	Key              string  `json:"key"`
	ContextWindow    int     `json:"context_window,omitempty"`
	MaxOutputTokens  int     `json:"max_output_tokens,omitempty"`
	InputUSDPerMTok  float64 `json:"input_usd_per_mtok,omitempty"`
	OutputUSDPerMTok float64 `json:"output_usd_per_mtok,omitempty"`
	Reasoning        *bool   `json:"reasoning,omitempty"`
	ToolCall         *bool   `json:"tool_call,omitempty"`
	Temperature      *bool   `json:"temperature,omitempty"`
}

var (
	snapshotOnce  sync.Once
	snapshotHdr   SnapshotHeader
	snapshotIndex map[string]Spec
	snapshotErr   error
)

// loadFrom is the production READ of a snapshot file: the parse, plus the
// three refusals that decide whether anything may be served from it — a
// schema this binary does not read, a header it cannot parse, a line it
// cannot parse. It returns no table with any of them: a file that is refused
// answers nothing, rather than answering for the rows that happened to parse.
//
// The schema verdict lives HERE, not in the parser: production refuses a file
// it cannot read as this schema, while the generator's changelog diffs the
// VALUES across a schema bump — which is when a reviewer needs them most.
//
// It exists as a seam because loadSnapshot is welded to the embedded blob and
// to this package's sync.Once state: a refusal reachable only by replacing
// the embedded file is a refusal no test can witness, and ADR-122's "schema 1
// is not kept loadable" rests on exactly this line. An accepted schema-1 file
// IS #2842 — its rows answer, and every capability flag reads unknown where
// the file states nothing.
func loadFrom(raw []byte) (SnapshotHeader, map[string]Spec, error) {
	hdr, index, err := ReadSnapshot(raw)
	if err != nil {
		return SnapshotHeader{}, nil, err
	}
	if hdr.Schema != SnapshotSchema {
		return SnapshotHeader{}, nil, fmt.Errorf("model specs: snapshot schema %d, want %d — regenerate with `task models:snapshot`", hdr.Schema, SnapshotSchema)
	}
	return hdr, index, nil
}

func loadSnapshot() {
	hdr, index, err := loadFrom([]byte(snapshotBlob))
	if err != nil {
		snapshotErr = err
		// A corrupted or wrong-schema snapshot must not fail SILENTLY into
		// false-unknown answers: the operator regenerating it is the fix,
		// and only a log line can ask for that.
		iterlog.NewFromEnv(os.Stderr).Warn("%v", err)
		return
	}
	snapshotHdr, snapshotIndex = hdr, index
}

// ReadSnapshot parses a snapshot file into its header and its table. It is
// the reader beside WriteSnapshot, so the format has exactly ONE definition:
// a field the writer emits cannot be dropped by a second parser, and the
// generator's changelog sees every field the file carries. It does not judge
// the schema — the caller does (loadSnapshot refuses a mismatch).
//
// It parses the bytes it is GIVEN. The embedded snapshot stays reachable only
// through SnapshotLookupExact's exact lookup (ADR-122 §4, Access): the blob is
// unexported, so this function cannot hand anyone the embedded table, and a
// reader wanting a whole table still goes through the live registry.
func ReadSnapshot(raw []byte) (SnapshotHeader, map[string]Spec, error) {
	hdrLine, body, _ := strings.Cut(string(raw), "\n")
	var hdr SnapshotHeader
	if err := json.Unmarshal([]byte(hdrLine), &hdr); err != nil {
		return SnapshotHeader{}, nil, fmt.Errorf("model specs: snapshot header: %w", err)
	}
	index := map[string]Spec{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e snapshotEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return SnapshotHeader{}, nil, fmt.Errorf("model specs: snapshot entry: %w", err)
		}
		index[e.Key] = Spec{
			ContextWindow:   e.ContextWindow,
			MaxOutputTokens: e.MaxOutputTokens,
			InputCostPerM:   e.InputUSDPerMTok,
			OutputCostPerM:  e.OutputUSDPerMTok,
			Reasoning:       e.Reasoning,
			ToolCall:        e.ToolCall,
			Temperature:     e.Temperature,
		}
	}
	return hdr, index, nil
}

// SnapshotLookupExact answers an exact "<provider>/<model>" lookup from the
// embedded snapshot — exact after the same lower-casing/trimming every
// lookup here applies; no bare id, suffix or prefix fallback exists. The
// snapshot is reachable ONLY through this function: a reader wanting a
// whole table must go through the live registry.
func SnapshotLookupExact(provider, modelID string) (Spec, bool) {
	snapshotOnce.Do(loadSnapshot)
	if snapshotErr != nil || snapshotIndex == nil {
		return Spec{}, false
	}
	key := strings.ToLower(strings.TrimSpace(provider)) + "/" + strings.ToLower(strings.TrimSpace(modelID))
	spec, ok := snapshotIndex[key]
	return spec, ok
}

// SnapshotInfo reports the snapshot header and its entry count, for the
// catalog's provenance line and the staleness warning. Zero values when the
// snapshot is unreadable.
func SnapshotInfo() (SnapshotHeader, int) {
	snapshotOnce.Do(loadSnapshot)
	if snapshotErr != nil {
		return SnapshotHeader{}, 0
	}
	return snapshotHdr, len(snapshotIndex)
}

// SnapshotAge is how old the snapshot's as_of is. Zero (and ok=false) when
// unreadable or the timestamp does not parse.
func SnapshotAge() (time.Duration, bool) {
	hdr, _ := SnapshotInfo()
	if hdr.AsOf == "" {
		return 0, false
	}
	asOf, err := time.Parse(time.RFC3339, hdr.AsOf)
	if err != nil {
		return 0, false
	}
	return time.Since(asOf), true
}

// WriteSnapshot renders a fetched models.dev body as the canonical snapshot
// file: the header line, then one sorted entry per line. The generator
// (scripts/modelsnapshot) writes it; this lives here so the format has
// exactly one definition beside its reader.
func WriteSnapshot(body []byte, sourceDigest string, asOf time.Time) ([]byte, error) {
	flat, err := ParseModelsDev(body)
	if err != nil {
		return nil, err
	}
	hdr, err := json.Marshal(SnapshotHeader{
		Schema:       SnapshotSchema,
		Source:       Source,
		SourceDigest: sourceDigest,
		AsOf:         asOf.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(flat))
	for k := range flat {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.Write(hdr)
	b.WriteByte('\n')
	for _, k := range keys {
		sp := flat[k]
		line, err := json.Marshal(snapshotEntry{
			Key:              k,
			ContextWindow:    sp.ContextWindow,
			MaxOutputTokens:  sp.MaxOutputTokens,
			InputUSDPerMTok:  sp.InputCostPerM,
			OutputUSDPerMTok: sp.OutputCostPerM,
			Reasoning:        sp.Reasoning,
			ToolCall:         sp.ToolCall,
			Temperature:      sp.Temperature,
		})
		if err != nil {
			return nil, err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return []byte(b.String()), nil
}
