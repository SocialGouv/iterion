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
// not match refuses the file rather than guess.
const SnapshotSchema = 1

// SnapshotHeader is the file's first line.
type SnapshotHeader struct {
	Schema       int    `json:"schema"`
	Source       string `json:"source"`
	SourceDigest string `json:"source_digest"` // sha256 of the raw models.dev body the entries were parsed from
	AsOf         string `json:"as_of"`         // RFC3339 UTC
}

// snapshotEntry is one line: the four numeric fields the gateway catalog
// resolves, nothing else.
type snapshotEntry struct {
	Key              string  `json:"key"`
	ContextWindow    int     `json:"context_window,omitempty"`
	MaxOutputTokens  int     `json:"max_output_tokens,omitempty"`
	InputUSDPerMTok  float64 `json:"input_usd_per_mtok,omitempty"`
	OutputUSDPerMTok float64 `json:"output_usd_per_mtok,omitempty"`
}

var (
	snapshotOnce  sync.Once
	snapshotHdr   SnapshotHeader
	snapshotIndex map[string]Spec
	snapshotErr   error
)

func loadSnapshot() {
	hdrLine, body, _ := strings.Cut(snapshotBlob, "\n")
	fail := func(err error) {
		snapshotErr = err
		// A corrupted or wrong-schema snapshot must not fail SILENTLY into
		// false-unknown answers: the operator regenerating it is the fix,
		// and only a log line can ask for that.
		iterlog.NewFromEnv(os.Stderr).Warn("%v", err)
	}
	if err := json.Unmarshal([]byte(hdrLine), &snapshotHdr); err != nil {
		fail(fmt.Errorf("model specs: snapshot header: %w", err))
		return
	}
	if snapshotHdr.Schema != SnapshotSchema {
		fail(fmt.Errorf("model specs: snapshot schema %d, want %d — regenerate with `task models:snapshot`", snapshotHdr.Schema, SnapshotSchema))
		return
	}
	snapshotIndex = map[string]Spec{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e snapshotEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			fail(fmt.Errorf("model specs: snapshot entry: %w", err))
			return
		}
		snapshotIndex[e.Key] = Spec{
			ContextWindow:   e.ContextWindow,
			MaxOutputTokens: e.MaxOutputTokens,
			InputCostPerM:   e.InputUSDPerMTok,
			OutputCostPerM:  e.OutputUSDPerMTok,
		}
	}
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
		})
		if err != nil {
			return nil, err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return []byte(b.String()), nil
}
