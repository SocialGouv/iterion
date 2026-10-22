package docsguard

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A page cannot track a constant that moves. `queue.SchemaVersion` is bumped
// by every change that adds a field to the wire, so a page that printed its
// value when it was written states a false accepted window — and the window is
// what an operator sequences a mixed-fleet deploy on. docs/dsl.md names the
// failure where it refuses to print the number: "read those two constants in
// pkg/queue/types.go … this page cannot track them, and a stale number here is
// the exact failure the queue rollout contract exists to prevent."
//
// So the guard is not "the number must be current": a page names the two
// constants and points at the file that declares them, and pins no value at
// all. A FLOOR is different and stays printable — the version at which a
// feature entered the wire never moves ("a snapshot-bearing message requires
// v13"), and the guard only reads a value written against the constants
// themselves.
//
// Dated records are out of reach: docs/bot-runs/ bilans and docs/reviews/
// audits report what was observed on their date, and an observation is not
// guidance.
//
// What it leaves out, deliberately — the C158 doctrine, a guard that misses is
// acceptable and one that lies is not: prose that describes the window without
// naming either constant ("a consumer accepts v10 through v24") is invisible
// to it. The anchored form is the one two operator-facing pages drifted on.
var queueVersionPin = regexp.MustCompile("`?queue\\.(?:Min)?SchemaVersion`?[^0-9A-Za-z\n]{0,12}[0-9]")

// queueProseSkipped are the trees whose pages are dated records.
var queueProseSkipped = []string{"bot-runs", "reviews"}

func TestNoGuidancePagePinsAValueForTheQueueSchemaConstants(t *testing.T) {
	root := filepath.Join("..", "..")
	types := forgeProseSource(t, "pkg", "queue", "types.go")
	for _, decl := range []string{"const SchemaVersion", "const MinSchemaVersion"} {
		if !strings.Contains(types, decl) {
			t.Fatalf("pkg/queue/types.go no longer declares `%s` — the pointer the pages give is fiction; move the guard and the pages with the constant", decl)
		}
	}
	for _, skipped := range queueProseSkipped {
		dir := filepath.Join(root, "docs", skipped)
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("docs/%s/ is not a directory (%v) — this guard excludes it as a tree of dated records; drop the exclusion rather than leave it fictional", skipped, err)
		}
	}
	if !queueVersionPin.MatchString("the wire version is `queue.SchemaVersion` = **24**") {
		t.Fatalf("the matcher no longer reads the pinned form it exists to refuse — every page below is unchecked")
	}

	var pinned, naming []string
	walked := 0
	err := filepath.WalkDir(filepath.Join(root, "docs"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			for _, skipped := range queueProseSkipped {
				if d.Name() == skipped {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		walked++
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if !strings.Contains(line, "queue.SchemaVersion") && !strings.Contains(line, "queue.MinSchemaVersion") {
				continue
			}
			naming = append(naming, rel)
			if m := queueVersionPin.FindString(line); m != "" {
				pinned = append(pinned, rel+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking docs/: %v", err)
	}
	if walked == 0 {
		t.Fatalf("no page read under docs/ — the tree moved and every claim below went unchecked")
	}
	if len(naming) == 0 {
		t.Fatalf("no page under docs/ names `queue.SchemaVersion` or `queue.MinSchemaVersion` (%d pages read) — the constants were renamed, or the guard is watching the wrong tree", walked)
	}
	for _, p := range pinned {
		t.Errorf("%s\n\tthis page pins a value for a constant it cannot track: name `queue.SchemaVersion` / `queue.MinSchemaVersion` and point at pkg/queue/types.go, as docs/dsl.md does, or print the FEATURE FLOOR, which never moves", p)
	}
}
