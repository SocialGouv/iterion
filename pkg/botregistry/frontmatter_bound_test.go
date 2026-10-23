package botregistry

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// allocatedBy is the number of bytes the heap handed out while f ran.
func allocatedBy(f func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// The catalogue the server lists reads every bot's frontmatter block, and
// holds it to the read budget by what its decode costs: a loose .bot whose
// block carries one dense line — some sixty records of the .bot — is listed
// under its file's name, its block not read, and listing it costs no more
// than a block of a plain line of the same length. Decoded, the line's 1.6
// million values would cost the listing some 500 MiB.
func TestADenseFrontmatterIsListedUnread(t *testing.T) {
	t.Setenv(parser.ReadBudgetEnv, "")
	botOf := func(line string) string {
		return "## ---\n## name: dense\n## x: " + line + "\n## ---\nagent x:\n  model: \"test\"\n"
	}
	list := func(src string) ([]Entry, uint64) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "probe.bot"), src)
		var entries []Entry
		var err error
		alloc := allocatedBy(func() { entries, err = List(ListOptions{Paths: []string{dir}}) })
		if err != nil {
			t.Fatal(err)
		}
		return entries, alloc
	}
	plainEntries, plain := list(botOf(strings.Repeat("ab", 1_600_001)))
	entries, dense := list(botOf("{" + strings.Repeat("a,", 1_600_000) + "a}"))
	t.Logf("listing allocated %d MiB, %d MiB for a plain line of the same length", dense>>20, plain>>20)
	if len(plainEntries) != 1 || plainEntries[0].Name != "dense" {
		t.Fatalf("the control is not read: %+v", plainEntries)
	}
	if len(entries) != 1 || entries[0].Name != "probe" {
		t.Fatalf("a bot whose frontmatter is past the budget is listed as %+v, want it under its file's name, its block unread", entries)
	}
	if dense > plain+64<<20 {
		t.Errorf("listing a dense frontmatter allocated %d MiB, a plain one %d MiB: the block was decoded", dense>>20, plain>>20)
	}
}
