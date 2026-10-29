package kubernetes

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// The entry names streamed into a pod are OPERANDS of the host tar: an
// attachment named like an option used to be one — `--checkpoint-action=exec=…`
// ran a command on the runner, the trusted side of the pipe. The fence is
// `--`; the witness is a directory whose entries are exactly those names.

// kubectlStdinShim records argv and keeps what every exec was piped.
func kubectlStdinShim(t *testing.T) (argvLog, stdinLog string) {
	t.Helper()
	isolateExecEnvironment(t)
	dir := t.TempDir()
	argvLog = filepath.Join(dir, "argv.log")
	stdinLog = filepath.Join(dir, "stdin.bin")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> \"$ITERION_TEST_KUBECTL_ARGV\"\n" +
		"case \"$*\" in\n" +
		"  *\" apply \"*) cat >> \"$ITERION_TEST_KUBECTL_ARGV.applied\"; printf '\\n---\\n' >> \"$ITERION_TEST_KUBECTL_ARGV.applied\"; exit 0 ;;\n" +
		"  *\" exec \"*\" tar \"*) cat >> \"$ITERION_TEST_KUBECTL_STDIN\"; exit 0 ;;\n" +
		"  *\" exec \"*) cat > /dev/null; exit 0 ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ITERION_TEST_KUBECTL_ARGV", argvLog)
	t.Setenv("ITERION_TEST_KUBECTL_STDIN", stdinLog)
	return argvLog, stdinLog
}

func tarMembers(t *testing.T, stream []byte) []string {
	t.Helper()
	var names []string
	tr := tar.NewReader(bytes.NewReader(stream))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("the exec stdin is not a tar stream: %v", err)
		}
		names = append(names, h.Name)
	}
	return names
}

func TestAnEntryNamedLikeAnOptionIsAnOperandOfTheHostTar(t *testing.T) {
	_, stdinLog := kubectlStdinShim(t)
	// GNU tar runs a checkpoint command in ITS working directory — this
	// test's, moved to a directory of its own so an unfenced run creates the
	// marker here and nowhere in the tree. An entry name holds no slash, so
	// the marker is a bare name.
	cwd := t.TempDir()
	t.Chdir(cwd)
	marker := filepath.Join(cwd, "PWNED")
	seed := t.TempDir()
	for _, name := range []string{"--checkpoint=1", "--checkpoint-action=exec=touch PWNED", "spec"} {
		if err := os.MkdirAll(filepath.Join(seed, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(seed, name, "f.md"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	d := &Driver{namespace: "ns", kubectl: "kubectl", logger: iterlog.New(iterlog.LevelInfo, io.Discard)}
	r := &Run{driver: d, podName: "p", namespace: "ns"}
	if err := r.streamDirIntoPod(context.Background(), seed, "/run/iterion/attachments"); err != nil {
		t.Fatalf("streamDirIntoPod: %v", err)
	}
	for _, m := range []string{marker, filepath.Join(seed, "PWNED")} {
		if _, err := os.Stat(m); err == nil {
			t.Fatalf("an entry name ran a command on the host: %s exists", m)
		}
	}
	stream, err := os.ReadFile(stdinLog)
	if err != nil {
		t.Fatalf("nothing was streamed: %v", err)
	}
	members := strings.Join(tarMembers(t, stream), " ")
	for _, want := range []string{"--checkpoint=1/f.md", "--checkpoint-action=exec=touch PWNED/f.md", "spec/f.md"} {
		if !strings.Contains(members, want) {
			t.Fatalf("the archive lacks %q: %s", want, members)
		}
	}
}

// What lands in the pod is the seed's CONTENTS at the top level, with the
// write bit taken off AFTER the copy — read from the stream and the order
// of the calls, not from the argv text alone.
func TestTheSeedLandsAtTheTopLevelAndIsMadeReadOnlyAfterTheCopy(t *testing.T) {
	argvLog, stdinLog := kubectlStdinShim(t)
	sched, err := schedulingFromEnv(envOf(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	d := &Driver{namespace: "ns", kubectl: "kubectl", logger: iterlog.New(iterlog.LevelInfo, io.Discard), sched: sched}
	seed := t.TempDir()
	if err := os.MkdirAll(filepath.Join(seed, "spec"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "spec", "spec.md"), []byte("# spec"), 0o644); err != nil {
		t.Fatal(err)
	}
	const target = "/run/iterion/attachments"
	prepared := &Prepared{spec: sandbox.Spec{Image: "img", User: "1000:1000"}, workspace: "/workspace"}
	if _, err := d.Start(context.Background(), prepared, sandbox.RunInfo{RunID: "01a0-run", SeedDirs: []sandbox.SeedDir{{HostDir: seed, ContainerPath: target, ReadOnly: true}}}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	stream, err := os.ReadFile(stdinLog)
	if err != nil {
		t.Fatalf("no seed was streamed: %v", err)
	}
	members := tarMembers(t, stream)
	top := false
	for _, m := range members {
		if m == "spec/spec.md" {
			top = true
		}
		if strings.HasPrefix(m, filepath.Base(seed)) {
			t.Fatalf("the seed's own directory was archived, so the files land one level deep: %v", members)
		}
	}
	if !top {
		t.Fatalf("spec/spec.md is not a top-level member: %v", members)
	}
	raw, _ := os.ReadFile(argvLog)
	argv := string(raw)
	tarAt := strings.Index(argv, "tar -C "+target)
	chmodAt := strings.Index(argv, "find "+target+" -type f -exec chmod a-w")
	if tarAt < 0 || chmodAt < 0 || chmodAt < tarAt {
		t.Fatalf("the write bit must come off AFTER the copy (tar at %d, chmod at %d):\n%s", tarAt, chmodAt, argv)
	}
}
