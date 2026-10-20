package bots

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

const deepsec239Fixture = "testdata/deepsec-2.3.9-export.json"

func readDeepsec239Fixture(t *testing.T) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(deepsec239Fixture)
	if err != nil {
		t.Fatal(err)
	}
	var findings []map[string]any
	if err := json.Unmarshal(raw, &findings); err != nil {
		t.Fatalf("decode the shared deepsec 2.3.9 fixture: %v", err)
	}
	if len(findings) != 4 {
		t.Fatalf("fixture carries %d findings, want HIGH/HIGH_BUG/BUG/LOW", len(findings))
	}
	for _, finding := range findings {
		for _, key := range []string{"title", "description", "severity", "labels"} {
			if _, ok := finding[key]; !ok {
				t.Fatalf("fixture finding lacks root %q: %v", key, finding)
			}
		}
		severity, _ := finding["severity"].(string)
		title, _ := finding["title"].(string)
		if !strings.HasPrefix(title, "["+severity+"] ") {
			t.Fatalf("fixture title %q is not prefixed by its severity %q", title, severity)
		}
		metadata, ok := finding["metadata"].(map[string]any)
		if !ok {
			t.Fatalf("fixture finding lacks metadata: %v", finding)
		}
		for _, key := range []string{"filePath", "lineNumbers", "vulnSlug", "confidence", "runId", "projectId", "severity", "discoveredAt", "owners"} {
			if _, ok := metadata[key]; !ok {
				t.Fatalf("fixture metadata lacks %q: %v", key, metadata)
			}
		}
		for _, key := range []string{"filePath", "vulnSlug", "confidence", "runId", "projectId", "severity", "discoveredAt"} {
			if _, ok := metadata[key].(string); !ok {
				t.Fatalf("fixture metadata.%s is not a string: %v", key, metadata[key])
			}
		}
		lineNumbers, ok := metadata["lineNumbers"].([]any)
		if !ok {
			t.Fatalf("fixture metadata.lineNumbers is not an array: %v", metadata["lineNumbers"])
		}
		for _, line := range lineNumbers {
			if _, ok := line.(float64); !ok {
				t.Fatalf("fixture metadata.lineNumbers contains a non-number: %v", line)
			}
		}
		if _, ok := finding["assignee"]; ok {
			t.Fatalf("fixture root assignee must not be derived by the fixture: %v", finding["assignee"])
		}
		owners, ok := metadata["owners"].(map[string]any)
		if !ok {
			t.Fatalf("fixture metadata.owners is not an OwnerSummary object: %v", metadata["owners"])
		}
		hasOwner := false
		if assignee, _ := owners["assignee"].(string); assignee != "" {
			hasOwner = true
		}
		for _, key := range []string{"teams", "oncall"} {
			entries, _ := owners[key].([]any)
			hasOwner = hasOwner || len(entries) > 0
		}
		labels, ok := finding["labels"].([]any)
		if !ok {
			t.Fatalf("fixture labels is not an array: %v", finding["labels"])
		}
		wantLabels := []string{
			"security",
			"project:" + metadata["projectId"].(string),
			"severity:" + severity,
			"slug:" + metadata["vulnSlug"].(string),
			"confidence:" + metadata["confidence"].(string),
		}
		if !hasOwner {
			wantLabels = append(wantLabels, "missing-owner")
		}
		if len(labels) != len(wantLabels) {
			t.Fatalf("fixture labels = %v, want %v (hasOwner=%v)", labels, wantLabels, hasOwner)
		}
		for i, want := range wantLabels {
			got, ok := labels[i].(string)
			if !ok || got != want {
				t.Fatalf("fixture labels[%d] = %v, want %q", i, labels[i], want)
			}
			if strings.HasPrefix(got, "owning-team:") && !hasOwner {
				t.Fatalf("fixture must not derive owning-team labels without owners: %v", labels)
			}
		}
		for _, key := range []string{"assignee", "assigneeSource"} {
			if _, ok := owners[key]; ok {
				t.Fatalf("fixture metadata.owners must not derive %s: %v", key, owners[key])
			}
		}
		ownerShapes := []struct {
			key      string
			required []string
			strings  []string
			numbers  []string
		}{
			{"teams", []string{"name", "slug"}, []string{"name", "slug"}, nil},
			{"oncall", []string{"name", "email"}, []string{"name", "email"}, nil},
			{"managers", []string{"email"}, []string{"email"}, nil},
			{"contributors", []string{"name", "email", "score"}, []string{"name", "email"}, []string{"score"}},
			{"recentCommitters", []string{"name", "email", "date"}, []string{"name", "email", "date"}, nil},
		}
		for _, shape := range ownerShapes {
			entries, ok := owners[shape.key].([]any)
			if !ok {
				t.Fatalf("fixture OwnerSummary.%s is not an array: %v", shape.key, owners[shape.key])
			}
			if len(entries) != 0 {
				t.Fatalf("fixture OwnerSummary.%s must stay empty rather than invent exporter derivatives: %v", shape.key, entries)
			}
			for _, entry := range entries {
				person, ok := entry.(map[string]any)
				if !ok {
					t.Fatalf("fixture OwnerSummary.%s contains a non-object: %v", shape.key, entry)
				}
				for _, key := range shape.required {
					if _, ok := person[key]; !ok {
						t.Fatalf("fixture OwnerSummary.%s entry lacks %q: %v", shape.key, key, person)
					}
				}
				for _, key := range shape.strings {
					if value, ok := person[key].(string); !ok || value == "" {
						t.Fatalf("fixture OwnerSummary.%s.%s is not a non-empty string: %v", shape.key, key, person[key])
					}
				}
				for _, key := range shape.numbers {
					if _, ok := person[key].(float64); !ok {
						t.Fatalf("fixture OwnerSummary.%s.%s is not a number: %v", shape.key, key, person[key])
					}
				}
			}
		}
		description, _ := finding["description"].(string)
		if len(description) <= 300 {
			t.Fatalf("fixture description must exercise the 300-character trim boundary: %v", title)
		}
		preamble, findingBody, found := strings.Cut(description, "## Finding")
		if !found || !strings.Contains(findingBody, "## Recommendation") || strings.Contains(description, "## Evidence") {
			t.Fatalf("fixture description does not use the real Finding/Recommendation sections: %v", title)
		}
		lineParts := make([]string, len(lineNumbers))
		for i, line := range lineNumbers {
			lineParts[i] = strconv.Itoa(int(line.(float64)))
		}
		fileLine := "**File:** `" + metadata["filePath"].(string) + "` (lines " + strings.Join(lineParts, ", ") + ")"
		projectLine := "**Project:** " + metadata["projectId"].(string)
		severityLine := fmt.Sprintf("**Severity:** %s  •  **Confidence:** %s  •  **Slug:** `%s`", severity, metadata["confidence"], metadata["vulnSlug"])
		lastIndex := -1
		for _, marker := range []string{fileLine, projectLine, severityLine} {
			index := strings.Index(preamble, marker)
			if index < 0 || index < lastIndex {
				t.Fatalf("fixture description preamble lacks or misorders %q: %q", marker, preamble)
			}
			lastIndex = index
		}
		if strings.Contains(preamble, "## Owners") {
			t.Fatalf("fixture description carries ## Owners although OwnerSummary is empty: %q", preamble)
		}
	}
	return findings
}

func runDeepsec239Cap(t *testing.T, scanDir, publishedPath string) capHarvest {
	t.Helper()
	scriptPath := filepath.Join(t.TempDir(), "cap.py")
	if err := os.WriteFile(scriptPath, []byte(capFindingsScript(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	paths, err := json.Marshal(map[string]string{"deepsec": publishedPath})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", scriptPath)
	cmd.Env = append(os.Environ(),
		"SCAN_DIR="+scanDir,
		"CAP=3",
		"INLINE_MAX=524288",
		"DEEPSEC_PATHS="+string(paths),
		"DEEPSEC_OUT="+filepath.Join(scanDir, "deepsec.json"),
	)
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("cap_findings exited non-zero (%v): %q", err, raw)
	}
	var got capHarvest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("cap_findings output is not JSON: %v (%q)", err, raw)
	}
	return got
}

func capGroup(t *testing.T, got capHarvest, file string) []map[string]any {
	t.Helper()
	for _, group := range got.Inline {
		if group.File == file {
			return group.Findings
		}
	}
	t.Fatalf("inline carries no %s group: %+v", file, got.Inline)
	return nil
}

func TestDeepsec239CapPreservesThePublishedProducerWithoutChangingOtherScanners(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	fixture := readDeepsec239Fixture(t)
	scanDir := filepath.Join(t.TempDir(), "scan")
	producerDir := filepath.Join(scanDir, "deepsec-out-run-239")
	if err := os.MkdirAll(producerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The producer identity is its published path, not a deepsec.json basename.
	publishedPath := filepath.Join(producerDir, "producer-export.json")
	writeJSON(t, publishedPath, fixture)

	longDescription := fixture[0]["description"].(string)
	writeJSON(t, filepath.Join(scanDir, "semgrep.json"), map[string]any{"results": []map[string]any{{
		"check_id": "other.rule",
		"severity": "HIGH",
		"extra": map[string]any{
			"message":  longDescription,
			"metadata": map[string]any{"filePath": "src/other.ts"},
			"lines":    strings.Repeat("code", 100),
		},
	}}})
	// A bare array from another scanner must keep the historical trimming even
	// though its shape matches deepsec's native export.
	writeJSON(t, filepath.Join(scanDir, "other-array.json"), []map[string]any{{
		"title":       "Other scanner array finding",
		"description": longDescription,
		"severity":    "HIGH",
		"metadata":    map[string]any{"filePath": "src/array.ts"},
	}})

	got := runDeepsec239Cap(t, scanDir, publishedPath)
	deepsec := capGroup(t, got, filepath.Base(publishedPath))
	if len(deepsec) != 3 {
		t.Fatalf("cap kept %d deepsec findings, want 3 of 4", len(deepsec))
	}
	byTitle := map[string]map[string]any{}
	for _, finding := range fixture {
		byTitle[finding["title"].(string)] = finding
	}
	for i, severity := range []string{"HIGH_BUG", "HIGH", "BUG"} {
		finding := deepsec[i]
		if gotSeverity, _ := finding["severity"].(string); gotSeverity != severity {
			t.Errorf("sorted finding %d has severity %q, want %q: %+v", i, gotSeverity, severity, deepsec)
			continue
		}
		if want := byTitle[finding["title"].(string)]; !reflect.DeepEqual(finding, want) {
			t.Errorf("%s was trimmed or re-indexed by cap_findings\ngot:  %#v\nwant: %#v", severity, finding, want)
		}
	}

	var onDisk []map[string]any
	raw, err := os.ReadFile(publishedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("the capped deepsec export is no longer a bare array: %v", err)
	}
	if !reflect.DeepEqual(onDisk, deepsec) {
		t.Errorf("deepsec file and inline transports diverged\nfile:   %#v\ninline: %#v", onDisk, deepsec)
	}

	semgrep := capGroup(t, got, "semgrep.json")
	extra, _ := semgrep[0]["extra"].(map[string]any)
	if _, ok := extra["metadata"]; ok {
		t.Error("semgrep metadata survived: non-deepsec trimming regressed")
	}
	if message, _ := extra["message"].(string); !strings.HasSuffix(message, "...[trunc]") {
		t.Errorf("semgrep long message was not trimmed: %q", message)
	}
	otherArray := capGroup(t, got, "other-array.json")
	if _, ok := otherArray[0]["metadata"]; ok {
		t.Error("another scanner's bare array was treated as deepsec and kept metadata")
	}
	if description, _ := otherArray[0]["description"].(string); !strings.HasSuffix(description, "...[trunc]") {
		t.Errorf("another scanner's bare array was not trimmed: %q", description)
	}
	semgrepRaw, err := os.ReadFile(filepath.Join(scanDir, "semgrep.json"))
	if err != nil {
		t.Fatal(err)
	}
	var semgrepFile struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(semgrepRaw, &semgrepFile); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(semgrepFile.Results, semgrep) {
		t.Errorf("semgrep file and inline trimming diverged\nfile:   %#v\ninline: %#v", semgrepFile.Results, semgrep)
	}
}

func TestDeepsec239CapUsesThePublishedInode(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	fixture := readDeepsec239Fixture(t)
	scanDir := filepath.Join(t.TempDir(), "scan")
	if err := os.MkdirAll(scanDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(scanDir, "z-target.json")
	publishedPath := filepath.Join(scanDir, "a-alias.json")
	writeJSON(t, target, fixture)
	if err := os.Symlink(target, publishedPath); err != nil {
		t.Skipf("symlink unavailable on this platform: %v", err)
	}
	// A second scanner also appears under two top-level names. Every glob entry
	// is deduped by inode, not just the producer path.
	otherTarget := filepath.Join(scanDir, "z-other.json")
	otherAlias := filepath.Join(scanDir, "a-other.json")
	writeJSON(t, otherTarget, map[string]any{"results": []map[string]any{
		{"check_id": "other.rule", "severity": "HIGH"},
		{"check_id": "other.rule", "severity": "MEDIUM"},
	}})
	if err := os.Symlink(otherTarget, otherAlias); err != nil {
		t.Skipf("symlink unavailable on this platform: %v", err)
	}

	got := runDeepsec239Cap(t, scanDir, publishedPath)
	if got.TotalRaw != 6 || got.TotalKept != 5 || got.TotalCapped != 1 || got.InlineTotal != 5 {
		t.Errorf("symlink aliases were harvested more than once: raw/kept/capped/inline_total = %d/%d/%d/%d, want 6/5/1/5",
			got.TotalRaw, got.TotalKept, got.TotalCapped, got.InlineTotal)
	}
	if len(got.PerScanner) != 2 {
		t.Errorf("per_scanner carries %d groups, want exactly the two inodes: %+v", len(got.PerScanner), got.PerScanner)
	}
	deepsec := capGroup(t, got, filepath.Base(publishedPath))
	if len(deepsec) != 3 || !reflect.DeepEqual(deepsec[0], fixture[1]) {
		t.Errorf("the published inode did not receive intact HIGH_BUG-first capping: %#v", deepsec)
	}
	if !strings.Contains(got.DeepsecHarvest, "3 of 4 findings harvested") {
		t.Errorf("deepsec harvest does not report the capped producer: %q", got.DeepsecHarvest)
	}
	otherGroups, otherFindings := 0, 0
	for _, group := range got.Inline {
		if group.File == filepath.Base(publishedPath) {
			continue
		}
		for _, finding := range group.Findings {
			if finding["check_id"] == "other.rule" {
				otherGroups++
				otherFindings += len(group.Findings)
				break
			}
		}
	}
	if otherGroups != 1 || otherFindings != 2 {
		t.Errorf("the non-deepsec symlink pair produced %d group(s)/%d finding(s), want 1/2", otherGroups, otherFindings)
	}
}

// Mutation: key the glob on realpath alone. The hardlink sorts before the
// producer, is treated as another scanner, and its trim rewrites the shared
// inode before deepsec's no-trim branch reads it.
func TestDeepsec239CapDedupesHardlinksBeforeANonDeepsecName(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	fixture := readDeepsec239Fixture(t)
	scanDir := filepath.Join(t.TempDir(), "scan")
	producerDir := filepath.Join(scanDir, "deepsec-out-run-239")
	if err := os.MkdirAll(producerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	producerPath := filepath.Join(producerDir, "z-producer.json")
	writeJSON(t, producerPath, fixture)
	hardlinkPath := filepath.Join(scanDir, "a-hardlink.json")
	if err := os.Link(producerPath, hardlinkPath); err != nil {
		t.Skipf("hardlink unavailable on this platform: %v", err)
	}

	got := runDeepsec239Cap(t, scanDir, producerPath)
	if got.TotalRaw != 4 || got.TotalKept != 3 || got.TotalCapped != 1 || got.InlineTotal != 3 {
		t.Errorf("hardlink was harvested twice: raw/kept/capped/inline_total = %d/%d/%d/%d, want 4/3/1/3",
			got.TotalRaw, got.TotalKept, got.TotalCapped, got.InlineTotal)
	}
	if len(got.PerScanner) != 1 {
		t.Errorf("per_scanner carries %d groups for one inode, want 1: %+v", len(got.PerScanner), got.PerScanner)
	}
	deepsec := capGroup(t, got, filepath.Base(producerPath))
	if len(deepsec) != 3 || !reflect.DeepEqual(deepsec[0], fixture[1]) {
		t.Errorf("the non-deepsec hardlink branch trimmed the shared inode first: %#v", deepsec)
	}
	var disk []map[string]any
	raw, err := os.ReadFile(hardlinkPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(disk, deepsec) {
		t.Errorf("hardlink disk and deepsec inline diverged\nfile:   %#v\ninline: %#v", disk, deepsec)
	}
}

func TestDeepsec239BankRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	fixture := readDeepsec239Fixture(t)
	exportPath := filepath.Join(t.TempDir(), "deepsec.json")
	writeJSON(t, exportPath, fixture)
	paths, err := json.Marshal(map[string]string{"deepsec": exportPath})
	if err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(t.TempDir(), "bank.py")
	if err := os.WriteFile(scriptPath, []byte(bankScript(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", scriptPath)
	cmd.Env = append(os.Environ(), "DEEPSEC_PATHS="+string(paths), "MAX_BYTES=524288")
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("bank_deepsec_findings exited non-zero (%v): %q", err, raw)
	}
	var got bankOut
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("bank output is not JSON: %v (%q)", err, raw)
	}
	if got.Total != 4 || got.Embedded != 4 || got.Truncated {
		t.Fatalf("bank envelope = %+v, want an untruncated four-finding round trip", got)
	}
	if !reflect.DeepEqual(got.Findings, fixture) {
		t.Errorf("bank changed the deepsec 2.3.9 export\ngot:  %#v\nwant: %#v", got.Findings, fixture)
	}
}

func TestDeepsecTriageMappingAppliesToInlineAndFileTransports(t *testing.T) {
	raw, err := os.ReadFile("sec-audit-source/main.bot")
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	start := strings.Index(body, "  - deepsec_scan:")
	end := strings.Index(body, "  - file_filter:")
	if start < 0 || end <= start {
		t.Fatal("cannot locate the deepsec triage instruction")
	}
	instruction := body[start:end]
	for _, want := range []string{"findings_budget.inline", "deepsec_scan.json_paths.deepsec", "read_file"} {
		if !strings.Contains(instruction, want) {
			t.Errorf("deepsec triage instruction does not cover %q: %s", want, instruction)
		}
	}
}

// Mutation: leave the skill on the pre-2.3.9 shape (root recommendation or an
// Evidence section) and this test reddens before triage can follow it.
func TestDeepsec239SkillDocumentsRealExport(t *testing.T) {
	raw, err := os.ReadFile("sec-audit-source/skills/scanner-deepsec.md")
	if err != nil {
		t.Fatal(err)
	}
	skill := string(raw)
	for _, want := range []string{
		"security", "project:<projectId>", "severity:<severity>", "slug:<vulnSlug>", "confidence:<confidence>", "missing-owner",
		"metadata.projectId",
		"metadata.severity",
		"metadata.discoveredAt",
		"metadata.owners",
		"teams", "oncall", "managers", "contributors", "recentCommitters",
		"{name, slug}", "{name, email", "{email}", "{name, email, score", "{name, email, date}",
		"**File:**", "` (lines ", "**Project:**", "**Severity:**", "**Confidence:**", "**Slug:**", "## Owners",
		"exporter supplied teams", "assignee` fields are exporter-derived", "empty OwnerSummary arrays mean no such section",
		"[SEVERITY]",
		"## Finding",
		"## Recommendation",
	} {
		if !strings.Contains(skill, want) {
			t.Errorf("scanner-deepsec skill does not document %q", want)
		}
	}
	if strings.Contains(skill, "## Evidence") {
		t.Error("scanner-deepsec still maps an Evidence section that the real 2.3.9 export does not carry")
	}
}

// Mutation: restore either stale catalogue sentence (two-phase judge or a
// lang_scan router edit) and both generated copies redden with the manifest.
func TestDeepsec239DocsAndCatalogsUseCurrentRuntime(t *testing.T) {
	paths := []string{
		"sec-audit-source/manifest.yaml",
		"sec-audit-source/skills/external-security-tools.md",
		"whats-next/skills/iterion-bot-catalog.md",
		"issue-triage/skills/iterion-bot-catalog.md",
		"../docs/security-bots.md",
		"../docs/references-bootstrap.md",
	}
	contents := map[string]string{}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		// Generated catalogues and YAML wrap prose; compare text, not line breaks.
		contents[path] = strings.Join(strings.Fields(string(raw)), " ")
		for _, stale := range []string{
			"two-phase judge (anti-façade)",
			"entry in the `lang_scan` router",
			"node $ITERION_REFERENCES_ROOT/deepsec/cli.mjs",
			"WARN once per run",
			"recognizes the export by the realpath",
			"selects deepsec by the realpath",
		} {
			if strings.Contains(contents[path], stale) {
				t.Errorf("%s still carries stale contract %q", path, stale)
			}
		}
	}
	for _, path := range []string{paths[0], paths[2], paths[3]} {
		for _, want := range []string{"three independent disprove voters", "`iterion:scanners` block", "deepsec CLI on PATH plus Node 22+", "OwnerSummary object", "Finding / Recommendation"} {
			if !strings.Contains(contents[path], want) {
				t.Errorf("%s does not carry %q", path, want)
			}
		}
	}
	for _, want := range []string{"never executes it as a fallback", "`deepsec` on `PATH`", "Node 22+", "scanner_output.errors[]", "inode identity", "OwnerSummary", "{name, slug}", "` (lines ", "## Owners", "exporter supplied teams", "exporter-derived", "oncall exists", "owner data exists"} {
		if !strings.Contains(contents[paths[1]], want) {
			t.Errorf("external-security-tools does not carry %q", want)
		}
	}
	for _, want := range []string{"Three-voter majority", "skill-driven language coverage", "`deepsec` CLI on `PATH` and Node 22+", "inode identity", "OwnerSummary", "{name, slug}", "` (lines ", "## Owners", "exporter supplied teams", "exporter-derived", "oncall exists", "owner data exists"} {
		if !strings.Contains(contents[paths[4]], want) {
			t.Errorf("security-bots does not carry %q", want)
		}
	}
	for _, want := range []string{"runtime executable is the installed CLI", "command -v deepsec", "never executes `cli.mjs`, `dist/cli.mjs`", "rebuilding the sec sandbox image", "only makes the diagnostic more precise"} {
		if !strings.Contains(contents[paths[5]], want) {
			t.Errorf("references-bootstrap does not carry %q", want)
		}
	}
}
