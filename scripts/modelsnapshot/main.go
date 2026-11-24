// Command modelsnapshot regenerates the embedded models.dev snapshot
// (pkg/backend/modelspecs/snapshot/models-dev.json): fetch, parse, render
// canonically, and print the changelog the PR carries. Never hand-edit the
// snapshot — this is its only writer.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

// dst is the one snapshot the generator writes and the binary embeds.
const dst = "pkg/backend/modelspecs/snapshot/models-dev.json"

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
// so the changelog can diff the regeneration against it. It reads through
// modelspecs.ReadSnapshot — the parser production uses — so a field the
// format carries cannot go missing from the changelog, and it ignores the
// file's SCHEMA on purpose: diffing the values across a schema bump is
// exactly what the reviewer of that bump needs.
func readCurrent() map[string]modelspecs.Spec {
	raw, err := os.ReadFile(dst)
	if err != nil {
		return map[string]modelspecs.Spec{}
	}
	_, table, err := modelspecs.ReadSnapshot(raw)
	if err != nil {
		// A snapshot already on disk that does not parse is said out loud:
		// the changelog would otherwise report the whole table as new and
		// the operator would read a corrupted file as a first generation.
		fmt.Fprintf(os.Stderr, "modelsnapshot: the snapshot on disk does not parse (%v) — the changelog treats it as empty\n", err)
		return map[string]modelspecs.Spec{}
	}
	return table
}

// changelogListCap bounds each printed list on its own. Per LIST, not per
// changelog: a regeneration that adds a field to every surviving entry would
// otherwise spend the whole budget on that one class and print nothing of the
// others — which is how the schema-2 bump reported 84% of the table changed,
// printed twenty flag arrivals, and hid 1112 numeric drifts behind
// "and N more", 22 of them a published price disappearing, 56 a context
// window shrinking and every scaleway price moving by the same 12.15%
// (#2842).
const changelogListCap = 20

func printChangelog(before, after map[string]modelspecs.Spec) {
	var added, removed, drifted, flagged []string
	for k := range after {
		old, ok := before[k]
		if !ok {
			added = append(added, k)
			continue
		}
		switch {
		case specsEqual(old, after[k]):
		case numbersEqual(old, after[k]):
			flagged = append(flagged, k)
		default:
			drifted = append(drifted, k)
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			removed = append(removed, k)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(drifted)
	sort.Strings(flagged)
	changed := len(drifted) + len(flagged)
	fmt.Printf("changelog: +%d -%d ~%d (total %d)\n", len(added), len(removed), changed, len(after))
	// The committed diff is the review — value flips are what a reviewer
	// needs to see, so changed entries print old→new, and a churn beyond a
	// fraction of the table is named out loud instead of scrolling past.
	if total := len(after); total > 0 && changed*100/total >= 10 {
		fmt.Printf("  !! %d of %d entries changed (%d%%) — verify this regeneration is intended\n", changed, total, changed*100/total)
	}
	// What CHANGED, split by what it costs to miss: a number that moved is a
	// price, a window or an output cap the engine now bills and compacts at;
	// a flag that arrived or left is a reader's verdict. The two counters are
	// printed together so neither class can be read as the whole churn.
	if changed > 0 {
		fmt.Printf("  ~ %d of the %d changed entries moved a NUMBER; %d changed flags only\n", len(drifted), changed, len(flagged))
	}
	printCapped("+", added, func(k string) string { return k })
	printCapped("-", removed, func(k string) string { return k })
	if len(drifted) > 0 {
		fmt.Println("  numbers drifted:")
		printCapped("~", drifted, func(k string) string {
			o, n := before[k], after[k]
			return fmt.Sprintf("%s: in %g→%g out %g→%g window %d→%d maxout %d→%d flags %s→%s", k,
				o.InputCostPerM, n.InputCostPerM, o.OutputCostPerM, n.OutputCostPerM,
				o.ContextWindow, n.ContextWindow, o.MaxOutputTokens, n.MaxOutputTokens,
				flagWords(o), flagWords(n))
		})
	}
	if len(flagged) > 0 {
		fmt.Println("  flags only:")
		printCapped("~", flagged, func(k string) string {
			return fmt.Sprintf("%s: flags %s→%s", k, flagWords(before[k]), flagWords(after[k]))
		})
	}
}

// printCapped prints at most changelogListCap entries of keys through line,
// then one line naming how many it did not print. One printer for the four
// lists, so a list added later cannot come with its own forgotten cap.
func printCapped(marker string, keys []string, line func(string) string) {
	for i, k := range keys {
		if i >= changelogListCap {
			fmt.Printf("  %s … and %d more\n", marker, len(keys)-changelogListCap)
			return
		}
		fmt.Printf("  %s %s\n", marker, line(k))
	}
}

// numbersEqual compares the four numbers the engine BILLS and compacts at —
// the two prices, the context window and the output cap. It is what separates
// a flag-only change from a value move; the flags are compared by specsEqual.
func numbersEqual(a, b modelspecs.Spec) bool {
	return a.ContextWindow == b.ContextWindow &&
		a.MaxOutputTokens == b.MaxOutputTokens &&
		a.InputCostPerM == b.InputCostPerM &&
		a.OutputCostPerM == b.OutputCostPerM
}

// specsEqual compares two entries by VALUE. Spec carries the three
// capability flags as pointers, and `!=` on a struct holding pointers
// compares ADDRESSES: two entries both stating `tool_call:false` would read
// as a flip, and every regeneration would report the whole table as changed.
func specsEqual(a, b modelspecs.Spec) bool {
	return numbersEqual(a, b) &&
		flagEqual(a.Reasoning, b.Reasoning) &&
		flagEqual(a.ToolCall, b.ToolCall) &&
		flagEqual(a.Temperature, b.Temperature)
}

// flagEqual compares two optional flags by value: unstated equals unstated,
// and a stated value equals only the same stated value.
func flagEqual(a, b *bool) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// flagWords renders the three flags for the changelog in the order the
// snapshot and the operator table name them: reasoning, tool_call,
// temperature — `?` for a flag the source did not state.
func flagWords(s modelspecs.Spec) string {
	return flagWord(s.Reasoning) + "/" + flagWord(s.ToolCall) + "/" + flagWord(s.Temperature)
}

func flagWord(f *bool) string {
	switch {
	case f == nil:
		return "?"
	case *f:
		return "yes"
	default:
		return "no"
	}
}

func shortDigest(d string) string {
	if len(d) > 19 {
		return d[:19] + "…"
	}
	return d
}
