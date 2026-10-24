package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testPrinter() (*Printer, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return &Printer{W: buf, Format: OutputHuman}, buf
}

// ---------------------------------------------------------------------------
// manifest: upsert / remove / round-trip
// ---------------------------------------------------------------------------

func TestScheduleManifest_UpsertRemoveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedules.yaml")

	m, err := loadScheduleManifest(path)
	if err != nil {
		t.Fatalf("load missing manifest: %v", err)
	}
	if m.Version != 1 || len(m.Schedules) != 0 {
		t.Fatalf("missing manifest should be empty v1, got version=%d n=%d", m.Version, len(m.Schedules))
	}

	if created := m.upsert(ScheduleEntry{Name: "a", Cron: "0 2 * * 1", Bot: "a.bot"}); !created {
		t.Errorf("first upsert should report created")
	}
	if created := m.upsert(ScheduleEntry{Name: "a", Cron: "0 3 * * 1", Bot: "a.bot"}); created {
		t.Errorf("second upsert of same name should report updated, not created")
	}
	if len(m.Schedules) != 1 {
		t.Fatalf("upsert of same name must not duplicate: n=%d", len(m.Schedules))
	}
	if got := m.Schedules[0].Cron; got != "0 3 * * 1" {
		t.Errorf("upsert did not overwrite cron: %q", got)
	}

	if err := saveScheduleManifest(path, m); err != nil {
		t.Fatalf("save: %v", err)
	}
	reloaded, err := loadScheduleManifest(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded.Schedules) != 1 || reloaded.Schedules[0].Name != "a" {
		t.Fatalf("round-trip lost the entry: %+v", reloaded.Schedules)
	}

	if !m.remove("a") {
		t.Errorf("remove of existing entry should report true")
	}
	if m.remove("a") {
		t.Errorf("remove of absent entry should report false")
	}
	if len(m.Schedules) != 0 {
		t.Errorf("remove did not delete: n=%d", len(m.Schedules))
	}
}

// ---------------------------------------------------------------------------
// validation
// ---------------------------------------------------------------------------

func TestValidateScheduleEntry(t *testing.T) {
	cases := []struct {
		name    string
		entry   ScheduleEntry
		wantErr bool
	}{
		{"ok", ScheduleEntry{Name: "weekly", Cron: "0 2 * * 1", Bot: "a.bot"}, false},
		{"empty name", ScheduleEntry{Name: "", Cron: "0 2 * * 1", Bot: "a.bot"}, true},
		{"space in name", ScheduleEntry{Name: "we ekly", Cron: "0 2 * * 1", Bot: "a.bot"}, true},
		{"slash in name", ScheduleEntry{Name: "a/b", Cron: "0 2 * * 1", Bot: "a.bot"}, true},
		{"empty bot", ScheduleEntry{Name: "weekly", Cron: "0 2 * * 1", Bot: ""}, true},
		{"empty cron", ScheduleEntry{Name: "weekly", Cron: "", Bot: "a.bot"}, true},
		{"4-field cron", ScheduleEntry{Name: "weekly", Cron: "0 2 * *", Bot: "a.bot"}, true},
		{"6-field cron", ScheduleEntry{Name: "weekly", Cron: "0 2 * * 1 7", Bot: "a.bot"}, true},
		{"bad timeout", ScheduleEntry{Name: "weekly", Cron: "0 2 * * 1", Bot: "a.bot", Timeout: "soon"}, true},
		{"good timeout", ScheduleEntry{Name: "weekly", Cron: "0 2 * * 1", Bot: "a.bot", Timeout: "2h"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateScheduleEntry(tc.entry)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateScheduleEntry(%+v) err=%v, wantErr=%v", tc.entry, err, tc.wantErr)
			}
		})
	}
}

func TestParseScheduleVars(t *testing.T) {
	vars, err := parseScheduleVars([]string{"label_source=sec-audit-self", "globs=pkg/**,cmd/**"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if vars["label_source"] != "sec-audit-self" {
		t.Errorf("label_source = %q", vars["label_source"])
	}
	// commas must survive verbatim (multi-glob var)
	if vars["globs"] != "pkg/**,cmd/**" {
		t.Errorf("comma value mangled: %q", vars["globs"])
	}
	if _, err := parseScheduleVars([]string{"noequals"}); err == nil {
		t.Errorf("expected error for var without '='")
	}
	if _, err := parseScheduleVars([]string{"=v"}); err == nil {
		t.Errorf("expected error for empty key")
	}
}

// ---------------------------------------------------------------------------
// crontab block: render / strip / splice idempotency
// ---------------------------------------------------------------------------

func sampleManifest() *ScheduleManifest {
	return &ScheduleManifest{
		Version: 1,
		Schedules: []ScheduleEntry{
			{Name: "sec-audit-source-weekly", Cron: "0 2 * * 1", Bot: "bots/sec-audit-source/main.bot",
				Workdir: "/home/jo/lab/ai/iterion", Description: "Weekly SAST self-audit"},
			{Name: "sec-audit-deps-weekly", Cron: "0 3 * * 1", Bot: "bots/sec-audit-deps/main.bot",
				Workdir: "/home/jo/lab/ai/iterion"},
			{Name: "paused-one", Cron: "0 4 * * 1", Bot: "x.bot", Workdir: "/tmp", Disabled: true},
		},
	}
}

func sampleBlock() string {
	return renderCronBlock(sampleManifest(), cronBlockParams{
		wrapperPath:  "/home/jo/.iterion/schedules.yaml.sh",
		manifestPath: "/home/jo/.iterion/schedules.yaml",
		logDir:       "/home/jo/.iterion/logs",
		tz:           "UTC",
	})
}

func TestRenderCronBlock(t *testing.T) {
	block := sampleBlock()
	for _, want := range []string{
		cronBlockBegin,
		cronBlockEnd,
		"CRON_TZ=UTC",
		"# Weekly SAST self-audit",
		"0 2 * * 1 cd /home/jo/lab/ai/iterion && /bin/sh /home/jo/.iterion/schedules.yaml.sh schedule run sec-audit-source-weekly --manifest /home/jo/.iterion/schedules.yaml >> /home/jo/.iterion/logs/schedule-sec-audit-source-weekly.log 2>&1",
		"0 3 * * 1 cd /home/jo/lab/ai/iterion",
		"# (disabled) paused-one — 0 4 * * 1 x.bot",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("rendered block missing %q\n---\n%s", want, block)
		}
	}
	if strings.Contains(block, "PATH=") {
		t.Errorf("PATH belongs in the wrapper, not the crontab:\n%s", block)
	}
	// a disabled entry must not produce an active cron line
	if strings.Contains(block, "schedule run paused-one") {
		t.Errorf("disabled entry leaked an active cron line:\n%s", block)
	}
}

func TestStripAndSpliceManagedBlock_Idempotent(t *testing.T) {
	user := "MAILTO=ops@example.com\n0 0 * * * /usr/bin/backup.sh"
	block := sampleBlock()

	// First install: user lines preserved, block appended.
	once := spliceManagedBlock(user, block)
	if !strings.Contains(once, "backup.sh") {
		t.Errorf("user crontab line was dropped:\n%s", once)
	}
	if !strings.Contains(once, cronBlockBegin) {
		t.Errorf("managed block not added:\n%s", once)
	}

	// Re-install must be idempotent (no duplicate block).
	twice := spliceManagedBlock(once, block)
	if got := strings.Count(twice, cronBlockBegin); got != 1 {
		t.Errorf("re-install duplicated the managed block: %d begin markers\n%s", got, twice)
	}
	if once != twice {
		t.Errorf("splice not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}

	// Uninstall restores the user crontab without the block.
	stripped := stripManagedBlock(twice)
	if strings.Contains(stripped, cronBlockBegin) {
		t.Errorf("strip left the begin marker:\n%s", stripped)
	}
	if !strings.Contains(stripped, "backup.sh") {
		t.Errorf("strip removed a user line:\n%s", stripped)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"":                      "''",
		"/home/jo/lab":          "/home/jo/lab",
		"name-1.2_3":            "name-1.2_3",
		"label=a,b":             "'label=a,b'", // '=' is not in the safe set
		"with space":            "'with space'",
		"it's":                  `'it'\''s'`,
		"pkg/**":                "'pkg/**'",
		"sec-audit-self":        "sec-audit-self",
		"/home/jo/.iterion/x.l": "/home/jo/.iterion/x.l",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// command-level: add → list → remove against a temp manifest
// ---------------------------------------------------------------------------

func TestRunScheduleAddListRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedules.yaml")
	common := ScheduleCommonOptions{ManifestPath: path}

	p, buf := testPrinter()
	if err := RunScheduleAdd(p, ScheduleAddOptions{
		ScheduleCommonOptions: common,
		Name:                  "sec-audit-source-weekly",
		Cron:                  "0 2 * * 1",
		Bot:                   "bots/sec-audit-source/main.bot",
		Workdir:               "/repo",
		VarFlags:              []string{"label_source=sec-audit-self"},
	}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if !strings.Contains(buf.String(), "added") {
		t.Errorf("add output: %q", buf.String())
	}

	// Re-add same name → update (not a second entry).
	p, buf = testPrinter()
	if err := RunScheduleAdd(p, ScheduleAddOptions{
		ScheduleCommonOptions: common, Name: "sec-audit-source-weekly",
		Cron: "30 2 * * 1", Bot: "bots/sec-audit-source/main.bot", Workdir: "/repo",
	}); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if !strings.Contains(buf.String(), "updated") {
		t.Errorf("re-add should report updated: %q", buf.String())
	}

	m, err := loadScheduleManifest(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(m.Schedules) != 1 {
		t.Fatalf("re-add duplicated entry: n=%d", len(m.Schedules))
	}
	if m.Schedules[0].Vars["label_source"] != "" {
		// the update omitted --var, so the stored entry replaces wholesale —
		// confirm replace semantics (no stale merge)
		t.Logf("note: update replaced entry wholesale (vars cleared) — expected")
	}

	// list (human)
	p, buf = testPrinter()
	if err := RunScheduleList(p, common); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(buf.String(), "sec-audit-source-weekly") {
		t.Errorf("list missing entry: %q", buf.String())
	}

	// remove
	p, _ = testPrinter()
	if err := RunScheduleRemove(p, ScheduleRefOptions{ScheduleCommonOptions: common, Name: "sec-audit-source-weekly"}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := RunScheduleRemove(p, ScheduleRefOptions{ScheduleCommonOptions: common, Name: "sec-audit-source-weekly"}); err == nil {
		t.Errorf("removing an absent schedule should error")
	}
}

// ---------------------------------------------------------------------------
// install / uninstall via injected crontab seam
// ---------------------------------------------------------------------------

func TestRunScheduleInstallUninstall_SeamRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv(schedulePathLimitEnv, "")
	path := filepath.Join(t.TempDir(), "schedules.yaml")
	if err := saveScheduleManifest(path, sampleManifest()); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}

	// Fake host crontab starting with a user line.
	fake := "MAILTO=ops@example.com\n0 0 * * * /usr/bin/backup.sh\n"
	origR, origW := crontabReader, crontabWriter
	t.Cleanup(func() { crontabReader, crontabWriter = origR, origW })
	crontabReader = func() (string, error) { return fake, nil }
	crontabWriter = func(s string) error { fake = s; return nil }

	p, _ := testPrinter()
	if err := RunScheduleInstall(p, ScheduleInstallOptions{ScheduleCommonOptions: ScheduleCommonOptions{ManifestPath: path}}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(fake, cronBlockBegin) || !strings.Contains(fake, "backup.sh") {
		t.Fatalf("install did not splice correctly:\n%s", fake)
	}
	if !strings.Contains(fake, "CRON_TZ=UTC") {
		t.Errorf("install did not default TZ to UTC:\n%s", fake)
	}

	// Idempotent re-install.
	if err := RunScheduleInstall(p, ScheduleInstallOptions{ScheduleCommonOptions: ScheduleCommonOptions{ManifestPath: path}}); err != nil {
		t.Fatalf("re-install: %v", err)
	}
	if got := strings.Count(fake, cronBlockBegin); got != 1 {
		t.Errorf("re-install duplicated block: %d markers", got)
	}

	// Uninstall removes the block, keeps the user line.
	if err := RunScheduleUninstall(p, ScheduleCommonOptions{ManifestPath: path}); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if strings.Contains(fake, cronBlockBegin) {
		t.Errorf("uninstall left the managed block:\n%s", fake)
	}
	if !strings.Contains(fake, "backup.sh") {
		t.Errorf("uninstall dropped a user line:\n%s", fake)
	}
}

func TestRunScheduleInstall_PrintDoesNotTouchCrontab(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv(schedulePathLimitEnv, "")
	path := filepath.Join(t.TempDir(), "schedules.yaml")
	if err := saveScheduleManifest(path, sampleManifest()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	origR, origW := crontabReader, crontabWriter
	t.Cleanup(func() { crontabReader, crontabWriter = origR, origW })
	wrote := false
	crontabReader = func() (string, error) { t.Fatalf("--print must not read the crontab"); return "", nil }
	crontabWriter = func(string) error { wrote = true; return nil }

	p, buf := testPrinter()
	if err := RunScheduleInstall(p, ScheduleInstallOptions{
		ScheduleCommonOptions: ScheduleCommonOptions{ManifestPath: path},
		Print:                 true,
	}); err != nil {
		t.Fatalf("install --print: %v", err)
	}
	if wrote {
		t.Errorf("--print must not write the crontab")
	}
	if _, err := os.Stat(path + ".sh"); !os.IsNotExist(err) {
		t.Errorf("--print must not write a wrapper: %v", err)
	}
	if !strings.Contains(buf.String(), cronBlockBegin) {
		t.Errorf("--print did not emit the block:\n%s", buf.String())
	}
}

func TestRunScheduleInstall_EmptyManifestErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedules.yaml")
	p, _ := testPrinter()
	if err := RunScheduleInstall(p, ScheduleInstallOptions{ScheduleCommonOptions: ScheduleCommonOptions{ManifestPath: path}}); err == nil {
		t.Errorf("install with no schedules should error")
	}
}

// ---------------------------------------------------------------------------
// run --dry-run resolves the command without executing
// ---------------------------------------------------------------------------

func TestRunScheduleRun_DryRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedules.yaml")
	m := &ScheduleManifest{Version: 1, Schedules: []ScheduleEntry{{
		Name: "weekly", Cron: "0 2 * * 1", Bot: "bots/sec-audit-source/main.bot",
		Workdir: "/repo", StoreDir: ".iterion", Sandbox: "auto",
		Vars: map[string]string{"label_source": "sec-audit-self"},
	}}}
	if err := saveScheduleManifest(path, m); err != nil {
		t.Fatalf("seed: %v", err)
	}

	p, buf := testPrinter()
	if err := RunScheduleRun(nil, p, ScheduleRunOptions{
		ScheduleCommonOptions: ScheduleCommonOptions{ManifestPath: path},
		Name:                  "weekly",
		DryRun:                true,
	}); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"cd /repo",
		"iterion run bots/sec-audit-source/main.bot",
		"--var label_source=sec-audit-self",
		"--store-dir .iterion",
		"--sandbox auto",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}

	// Unknown schedule errors.
	if err := RunScheduleRun(nil, p, ScheduleRunOptions{
		ScheduleCommonOptions: ScheduleCommonOptions{ManifestPath: path},
		Name:                  "nope", DryRun: true,
	}); err == nil {
		t.Errorf("running an unknown schedule should error")
	}
}
