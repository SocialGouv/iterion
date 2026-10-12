// Command modelsnapshot regenerates the embedded models.dev snapshot
// (pkg/backend/modelspecs/snapshot/models-dev.json): fetch, parse, render
// canonically, and print the changelog the PR carries. Never hand-edit the
// snapshot — this is its only writer.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "modelsnapshot:", err)
		os.Exit(1)
	}
}

func run() error {
	before := readCurrent()
	body, digest, err := fetch()
	if err != nil {
		return err
	}
	out, err := modelspecs.WriteSnapshot(body, digest, time.Now().UTC())
	if err != nil {
		return err
	}
	dst := "pkg/backend/modelspecs/snapshot/models-dev.json"
	if werr := os.WriteFile(dst, out, 0o644); werr != nil {
		return werr
	}
	after := readCurrent()
	printChangelog(before, after)
	fmt.Printf("modelsnapshot: wrote %s (%d bytes, source digest %s)\n", dst, len(out), shortDigest(digest))
	return nil
}

func fetch() ([]byte, string, error) {
	req, err := http.NewRequest("GET", modelspecs.Source, nil)
	if err != nil {
		return nil, "", err
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("models.dev: unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(body)
	return body, "sha256:" + hex.EncodeToString(sum[:]), nil
}

// readCurrent parses the snapshot on disk (or an empty table when absent)
// so the changelog can diff the regeneration against it.
func readCurrent() map[string]modelspecs.Spec {
	out := map[string]modelspecs.Spec{}
	raw, err := os.ReadFile("pkg/backend/modelspecs/snapshot/models-dev.json")
	if err != nil {
		return out
	}
	lines := strings.Split(string(raw), "\n")
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e struct {
			Key              string  `json:"key"`
			ContextWindow    int     `json:"context_window"`
			MaxOutputTokens  int     `json:"max_output_tokens"`
			InputUSDPerMTok  float64 `json:"input_usd_per_mtok"`
			OutputUSDPerMTok float64 `json:"output_usd_per_mtok"`
		}
		if json.Unmarshal([]byte(line), &e) == nil && e.Key != "" {
			out[e.Key] = modelspecs.Spec{
				ContextWindow:   e.ContextWindow,
				MaxOutputTokens: e.MaxOutputTokens,
				InputCostPerM:   e.InputUSDPerMTok,
				OutputCostPerM:  e.OutputUSDPerMTok,
			}
		}
	}
	return out
}

func printChangelog(before, after map[string]modelspecs.Spec) {
	var added, removed, changed []string
	for k := range after {
		old, ok := before[k]
		if !ok {
			added = append(added, k)
			continue
		}
		if old != after[k] {
			changed = append(changed, k)
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			removed = append(removed, k)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)
	fmt.Printf("changelog: +%d -%d ~%d (total %d)\n", len(added), len(removed), len(changed), len(after))
	// The committed diff is the review — value flips are what a reviewer
	// needs to see, so changed entries print old→new, and a churn beyond a
	// fraction of the table is named out loud instead of scrolling past.
	if total := len(after); total > 0 && len(changed)*100/total >= 10 {
		fmt.Printf("  !! %d of %d entries changed (%d%%) — verify this regeneration is intended\n", len(changed), total, len(changed)*100/total)
	}
	for i, k := range added {
		if i >= 20 {
			fmt.Printf("  + … and %d more\n", len(added)-20)
			break
		}
		fmt.Printf("  + %s\n", k)
	}
	for i, k := range removed {
		if i >= 20 {
			fmt.Printf("  - … and %d more\n", len(removed)-20)
			break
		}
		fmt.Printf("  - %s\n", k)
	}
	for i, k := range changed {
		if i >= 20 {
			fmt.Printf("  ~ … and %d more\n", len(changed)-20)
			break
		}
		o, n := before[k], after[k]
		fmt.Printf("  ~ %s: in %g→%g out %g→%g window %d→%d\n", k,
			o.InputCostPerM, n.InputCostPerM, o.OutputCostPerM, n.OutputCostPerM,
			o.ContextWindow, n.ContextWindow)
	}
}

func shortDigest(d string) string {
	if len(d) > 19 {
		return d[:19] + "…"
	}
	return d
}
