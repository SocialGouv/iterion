package kubernetes

import (
	"debug/elf"
	"fmt"
	"github.com/SocialGouv/iterion/internal/hometest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

var nativeFixture struct {
	sync.Once
	dir, host, capsule string
	err                error
}

func TestMain(m *testing.M) {
	os.Exit(hometest.IsolateUserHome(func() int {
		code := m.Run()
		if nativeFixture.dir != "" {
			_ = os.RemoveAll(nativeFixture.dir)
		}
		return code
	}))
}

// Build the actual application entry and native pair once per package run.
// A missing compiler on a supported platform is a failed proof, never a skip.
func nativeExecTransport(t *testing.T) (host, capsule string) {
	t.Helper()
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		t.Skip("native capsule supports Linux amd64/arm64")
	}
	nativeFixture.Do(func() {
		nativeFixture.err = buildNativeExecTransport()
	})
	if nativeFixture.err != nil {
		t.Fatal(nativeFixture.err)
	}
	return nativeFixture.host, nativeFixture.capsule
}

func buildNativeExecTransport() error {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		return fmt.Errorf("locate native fixture source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "../../.."))
	dir, err := os.MkdirTemp("", "iterion-execenv-native-")
	if err != nil {
		return err
	}
	nativeFixture.dir = dir
	nativeFixture.host = filepath.Join(dir, "iterion")
	nativeFixture.capsule = "/opt/capsule"
	capsuleFile := filepath.Join(dir, "capsule")
	guest := filepath.Join(dir, "guest")
	run := func(name string, args ...string) error {
		cmd := exec.Command(name, args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("native fixture %s: %w: %s", name, err, output)
		}
		return nil
	}
	flags := fmt.Sprintf("-X %q -X %q", "main.capsulePath="+nativeFixture.capsule, "main.payloadPath=/opt/guest")
	if err := run("go", "build", "-mod=vendor", "-ldflags", flags, "-o", guest, "./cmd/iterion-execenv-guest"); err != nil {
		return err
	}
	if err := run("cc", "-static", "-nostdlib", "-ffreestanding", "-fno-builtin", "-fno-stack-protector", "-fno-pie", "-Wl,-no-pie", "-O2", "-Wall", "-Wextra", "-Werror", "-DGUEST_PATH="+strconv.Quote("/opt/guest"), "-o", capsuleFile, "native/execenv/capsule.c"); err != nil {
		return err
	}
	for _, path := range []string{guest, capsuleFile} {
		f, err := elf.Open(path)
		if err != nil {
			return err
		}
		want := elf.EM_X86_64
		if runtime.GOARCH == "arm64" {
			want = elf.EM_AARCH64
		}
		if f.Machine != want || f.Type != elf.ET_EXEC {
			_ = f.Close()
			return fmt.Errorf("native fixture has wrong ELF target: %s", path)
		}
		for _, prog := range f.Progs {
			if prog.Type == elf.PT_INTERP || prog.Type == elf.PT_DYNAMIC {
				_ = f.Close()
				return fmt.Errorf("native fixture has a runtime loader: %s", path)
			}
		}
		_ = f.Close()
	}
	if err := run("go", "build", "-mod=vendor", "-o", nativeFixture.host, "./cmd/iterion"); err != nil {
		return err
	}
	bwrap := os.Getenv("ITERION_TEST_BWRAP")
	if bwrap == "" {
		bwrap, err = exec.LookPath("bwrap")
		if err != nil {
			return fmt.Errorf("native fixture needs bubblewrap: %w", err)
		}
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	// Bubblewrap is only the mount supervisor. It must survive the driver's
	// group TERM long enough for the real Go host to clean its client group.
	// The host's signal.NotifyContext explicitly re-enables TERM/INT handling.
	// SIGKILL still tears the supervisor down via --die-with-parent, so this
	// fixture cannot conceal a caller that overwrites the owned Cancel hook.
	wrapper := filepath.Join(dir, "host-relay")
	script := "#!/bin/sh\ntrap '' TERM INT\nexec " + quote(bwrap) + " --die-with-parent --ro-bind / / --ro-bind " + quote(dir) + " /opt --bind " + quote(os.TempDir()) + " " + quote(os.TempDir()) + " --dev /dev --proc /proc /opt/iterion \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		return err
	}
	nativeFixture.host = wrapper
	return nil
}
