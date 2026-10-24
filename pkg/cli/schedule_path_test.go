package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// W1: restore PATH=<install-time PATH> in renderCronBlock and the 1000-byte
// assertion fails. Bypass the wrapper PATH assignment and its content check fails.
func TestScheduleInstallLongPathLivesInAtomicWrapper(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	raw := "/" + strings.Repeat("a", 2490) + ":/usr/bin"
	if len(raw) != 2500 {
		t.Fatal("fixture must contain exactly 2500 bytes")
	}
	t.Setenv("PATH", raw)
	t.Setenv(schedulePathLimitEnv, "")
	path := filepath.Join(dir, "schedules.yaml")
	if err := saveScheduleManifest(path, sampleManifest()); err != nil {
		t.Fatal(err)
	}
	wrapperPath := path + ".sh"
	if err := os.WriteFile(wrapperPath, []byte("old wrapper"), 0o600); err != nil {
		t.Fatal(err)
	}
	old, err := os.Open(wrapperPath)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	origR, origW := crontabReader, crontabWriter
	t.Cleanup(func() { crontabReader, crontabWriter = origR, origW })
	crontabReader = func() (string, error) { return "", nil }
	var block string
	crontabWriter = func(s string) error {
		block = s
		body, err := os.ReadFile(wrapperPath)
		if err != nil {
			t.Fatalf("wrapper must exist before publishing cron: %v", err)
		}
		wantPath := raw + ":/usr/local/bin:/bin"
		for _, want := range []string{"#!/bin/sh\n", "PATH=" + wantPath + "\nexport PATH\n", "original=2500", fmt.Sprintf("sanitized=%d", len(wantPath)), "exec "} {
			if !strings.Contains(string(body), want) {
				t.Errorf("wrapper missing %q: %s", want, body)
			}
		}
		st, err := os.Stat(wrapperPath)
		if err != nil || st.Mode().Perm() != 0o700 {
			t.Errorf("wrapper must be private and executable: stat=%v, err=%v", st, err)
		}
		return nil
	}
	p, _ := testPrinter()
	if err := RunScheduleInstall(p, ScheduleInstallOptions{ScheduleCommonOptions: ScheduleCommonOptions{ManifestPath: path}}); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(block, "\n") {
		if len(line) >= 1000 {
			t.Errorf("cron line has %d bytes, want <1000", len(line))
		}
		if strings.HasPrefix(line, "PATH=") {
			t.Errorf("cron must not carry PATH: %s", line)
		}
	}
	if !strings.Contains(block, "/bin/sh "+wrapperPath+" schedule run") {
		t.Errorf("cron must invoke the wrapper: %s", block)
	}
	before, err := io.ReadAll(old)
	if err != nil || string(before) != "old wrapper" {
		t.Fatalf("wrapper must be replaced atomically, not overwritten in place: %q, %v", before, err)
	}
}

// W2: remove the empty/relative/duplicate checks, the missing-system append,
// or the protected-directory continue in the trimming loop: the matching row fails.
func TestSanitizeSchedulePathKeepsSystemTail(t *testing.T) {
	cases := []struct {
		name, raw, want string
		limit           int
		dropped         []droppedSchedulePath
	}{
		{"filter", ":/keep:relative:/keep:" + scheduleSystemPath + ":", "/keep:" + scheduleSystemPath, 8192,
			[]droppedSchedulePath{{"", "empty"}, {"relative", "relative"}, {"/keep", "duplicate"}, {"", "empty"}}},
		{"trim tail", "/keep:/drop-a:/drop-b:" + scheduleSystemPath, "/keep:" + scheduleSystemPath, len("/keep:" + scheduleSystemPath),
			[]droppedSchedulePath{{"/drop-b", "size limit"}, {"/drop-a", "size limit"}}},
		{"append system", "/keep:/usr/bin", "/keep:/usr/bin:/usr/local/bin:/bin", 8192, nil},
		{"empty", "", scheduleSystemPath, 8192, []droppedSchedulePath{{"", "empty"}}},
		{"controls", "/bad\npath:/bad\rpath:/bad\x00path", scheduleSystemPath, 8192,
			[]droppedSchedulePath{{"/bad\npath", "control character"}, {"/bad\rpath", "control character"}, {"/bad\x00path", "control character"}}},
		{"unlimited", "/keep:/drop-a:" + scheduleSystemPath, "/keep:/drop-a:" + scheduleSystemPath, 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, dropped := sanitizeSchedulePath(tc.raw, tc.limit)
			if got != tc.want || !reflect.DeepEqual(dropped, tc.dropped) {
				t.Fatalf("sanitize = %q, dropped=%v; want %q, dropped=%v", got, dropped, tc.want, tc.dropped)
			}
		})
	}
}

func TestScheduleInstallNamesDroppedPathEntries(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Chdir(dir)
	t.Setenv("PATH", ":relative:/keep:/keep:/drop:"+scheduleSystemPath)
	t.Setenv(schedulePathLimitEnv, fmt.Sprint(len("/keep:"+scheduleSystemPath)))
	if err := saveScheduleManifest("schedules.yaml", sampleManifest()); err != nil {
		t.Fatal(err)
	}
	origR, origW := crontabReader, crontabWriter
	t.Cleanup(func() { crontabReader, crontabWriter = origR, origW })
	crontabReader = func() (string, error) { return "", nil }
	crontabWriter = func(s string) error {
		if !strings.Contains(s, "--manifest "+filepath.Join(dir, "schedules.yaml")) {
			t.Errorf("cron must resolve a relative manifest before changing directories: %s", s)
		}
		return nil
	}
	p, buf := testPrinter()
	if err := RunScheduleInstall(p, ScheduleInstallOptions{ScheduleCommonOptions: ScheduleCommonOptions{ManifestPath: "schedules.yaml"}}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`PATH dropped "" (empty)`, `PATH dropped "relative" (relative)`, `PATH dropped "/keep" (duplicate)`, `PATH dropped "/drop" (size limit)`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("install must name discarded entries: missing %s in %s", want, buf)
		}
	}
}

func TestScheduleWrapperExecutesWithQuotedPathAndArguments(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("PATH", "/no-runtime-here")
	bin := filepath.Join(dir, "fake iterion's binary")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$PATH\" \"$@\"\nexit 23\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cleanPath := "/space and 'quote/$(false)/100%:" + scheduleSystemPath
	wrapper := filepath.Join(dir, "wrapper.sh")
	if err := writeFileAtomic(wrapper, []byte(renderScheduleWrapper(bin, cleanPath, cleanPath, 8192)), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/bin/sh", wrapper, "schedule", "run", "argument with 'quotes' and $variables").CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 23 {
		t.Fatalf("wrapper must preserve the binary exit status: %v, %s", err, out)
	}
	if want := cleanPath + "\nschedule\nrun\nargument with 'quotes' and $variables\n"; string(out) != want {
		t.Fatalf("wrapper changed PATH or argv: %q, want %q", out, want)
	}
}

func TestSchedulePathLimitOverride(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
		bad   bool
	}{{"", 8192, false}, {"0", 0, false}, {"32768", 32768, false}, {"-1", 0, true}, {"1", 0, true}, {"bad", 0, true}} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv(schedulePathLimitEnv, tc.value)
			got, err := schedulePathLimit()
			if (err != nil) != tc.bad || got != tc.want {
				t.Fatalf("limit = %d, %v; want %d, bad=%v", got, err, tc.want, tc.bad)
			}
		})
	}
}

// Mutation: return the crontabWriter error without restoring the wrapper.
func TestScheduleInstallRejectedCrontabRestoresWrapper(t *testing.T) {
	for _, existed := range []bool{false, true} {
		t.Run(fmt.Sprint(existed), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			t.Setenv("PATH", "/new-tools:/usr/bin:/bin")
			t.Setenv(schedulePathLimitEnv, "")
			path := filepath.Join(dir, "schedules.yaml")
			if err := saveScheduleManifest(path, sampleManifest()); err != nil {
				t.Fatal(err)
			}
			wrapperPath := path + ".sh"
			if existed {
				if err := os.WriteFile(wrapperPath, []byte("previous wrapper"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			origR, origW := crontabReader, crontabWriter
			t.Cleanup(func() { crontabReader, crontabWriter = origR, origW })
			crontabReader = func() (string, error) { return sampleBlock(), nil }
			rejected := errors.New("crontab rejected")
			crontabWriter = func(string) error { return rejected }
			p, _ := testPrinter()
			if err := RunScheduleInstall(p, ScheduleInstallOptions{ScheduleCommonOptions: ScheduleCommonOptions{ManifestPath: path}}); !errors.Is(err, rejected) {
				t.Fatalf("must report crontab failure: %v", err)
			}
			body, err := os.ReadFile(wrapperPath)
			if !existed {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed first install left wrapper: %q, %v", body, err)
				}
				return
			}
			if err != nil || string(body) != "previous wrapper" {
				t.Fatalf("failed reinstall changed active wrapper: %q, %v", body, err)
			}
			info, err := os.Stat(wrapperPath)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("failed reinstall changed wrapper permissions: %v, %v", info, err)
			}
		})
	}
}
