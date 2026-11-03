package stdiotest_test

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/stdiotest"
)

// A full pipe must not stop the operation being observed. The subprocess is
// killable so restoring the old deadlock fails THIS assertion, not the suite's
// outer timeout. Two MiB exceeds the ordinary maximum Linux pipe capacity.
func TestCaptureExceedsPipeCapacity(t *testing.T) {
	if os.Getenv("ITERION_STDIO_CAPTURE_CHILD") == "1" {
		payload := strings.Repeat("capture-pressure\n", 1<<17)
		for _, stream := range []**os.File{&os.Stdout, &os.Stderr} {
			got := stdiotest.Run(t, stream, func() {
				if _, err := (*stream).WriteString(payload); err != nil {
					t.Fatal(err)
				}
			})
			if got != payload {
				t.Fatalf("captured %d bytes, want all %d bytes unchanged", len(got), len(payload))
			}
		}
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestCaptureExceedsPipeCapacity$")
	cmd.Env = append(os.Environ(), "ITERION_STDIO_CAPTURE_CHILD=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("capture did not complete a write larger than pipe capacity: %s", out)
	}
	if err != nil {
		t.Fatalf("capture child: %v\n%s", err, out)
	}
}

func TestIntervalsAndNestedCaptureRestoreTheirStreams(t *testing.T) {
	original := os.Stderr
	c := stdiotest.Start(t, &os.Stderr)
	defer c.Close()
	outer := os.Stderr
	if _, err := os.Stderr.WriteString("before"); err != nil {
		t.Fatal(err)
	}
	nested := stdiotest.Run(t, &os.Stderr, func() {
		if _, err := os.Stderr.WriteString("inner"); err != nil {
			t.Fatal(err)
		}
	})
	if nested != "inner" || os.Stderr != outer {
		t.Fatal("nested capture lost text or did not restore its parent")
	}
	if _, err := os.Stderr.WriteString("after"); err != nil {
		t.Fatal(err)
	}
	if got := c.Read(); got != "beforeafter" {
		t.Fatalf("first interval = %q", got)
	}
	if got := c.Read(); got != "" {
		t.Fatalf("empty interval = %q", got)
	}
	if _, err := os.Stderr.WriteString("next"); err != nil {
		t.Fatal(err)
	}
	if got := c.Read(); got != "next" {
		t.Fatalf("next interval = %q", got)
	}
	// Close owns the unread tail too.
	if _, err := os.Stderr.WriteString("tail"); err != nil {
		t.Fatal(err)
	}
	lastWriter := os.Stderr
	c.Close()
	c.Close()
	if os.Stderr != original {
		t.Fatal("Close did not restore stderr")
	}
	if _, err := lastWriter.WriteString("closed"); err == nil {
		t.Fatal("Close leaked its writer")
	}
}

func TestUnwindingRestoresStream(t *testing.T) {
	original := os.Stdout
	func() {
		defer func() {
			if got := recover(); got != "sentinel" {
				t.Fatalf("panic = %v", got)
			}
		}()
		stdiotest.Run(t, &os.Stdout, func() { panic("sentinel") })
	}()
	if os.Stdout != original {
		t.Fatal("panic left stdout redirected")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		stdiotest.Run(t, &os.Stdout, func() { runtime.Goexit() })
	}()
	<-done
	if os.Stdout != original {
		t.Fatal("Goexit left stdout redirected")
	}
}

func TestTestCleanupRestoresUnreadCapture(t *testing.T) {
	original := os.Stderr
	var writer *os.File
	t.Run("capture without explicit close", func(t *testing.T) {
		stdiotest.Start(t, &os.Stderr)
		writer = os.Stderr
		if _, err := writer.WriteString("unread"); err != nil {
			t.Fatal(err)
		}
	})
	if os.Stderr != original {
		t.Fatal("test cleanup left stderr redirected")
	}
	if _, err := writer.WriteString("closed"); err == nil {
		t.Fatal("test cleanup leaked its writer")
	}
}
