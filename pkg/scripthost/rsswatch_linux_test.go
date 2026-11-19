//go:build linux

package scripthost

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The axis this probe exists for: a mapping RLIMIT_DATA does not charge. The
// premise is measured here rather than assumed — the SAME helper grows the
// same mapping twice, once under RLIMIT_DATA (and survives it) and once under
// the probe (and is killed).
func TestAMappingRLIMITDataDoesNotChargeGrowsTheResidentSet(t *testing.T) {
	// Premise: 96 MiB of MAP_SHARED with only 16 MiB of room left under
	// RLIMIT_DATA. Charged, the mapping alone would overrun the room and the
	// mmap would be refused (exit 3); the kernel does not charge it, so the
	// child finishes.
	//
	// ROOM, not an absolute limit: the helper sets its own data segment plus
	// 16 MiB. An absolute 32 MiB sat a few MiB above the test binary's own
	// ~22 MiB, and the runtime died whenever it needed a mapping after the
	// limit went up — a memory-profile sample asking for a persistentalloc
	// chunk, at random: red five times on CI in a day, 3 runs out of 40
	// locally under CPU contention (#3372).
	cmd, stderr := rssHelper(t, "96", map[string]string{"ITERION_RLIMIT_DATA_ROOM_MIB": "16"})
	if err := cmd.Wait(); err != nil {
		t.Fatalf("a MAP_SHARED mapping under RLIMIT_DATA died (%v) — the axis this probe covers "+
			"would not exist, and the probe's own verdict would be about something else. "+
			"Helper exits: 3 = the mmap was refused (charged), 5 = setrlimit, 6 = no base, 7 = bad room. "+
			"stderr: %.400s", err, stderr.Bytes())
	}
}

func TestTheProbeKillsAChildWhoseMappingRLIMITDataDoesNotCharge(t *testing.T) {
	cmd, _ := rssHelper(t, "96", nil)
	stop := make(chan struct{})
	done := watchRSS(cmd, 32<<20, 5*time.Millisecond, stop)
	// BOUNDED. A probe that never kills must FAIL this test, not hang it: the
	// first version blocked forever on a mutant that widened the comparison,
	// and a hanging test is worse than an absent one.
	var res probeResult
	select {
	case res = <-done:
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("the probe never killed a child holding 96 MiB against a 32 MiB bound")
	}
	close(stop)
	_ = cmd.Wait()

	if !res.killed {
		t.Fatalf("the probe did not kill a child holding 96 MiB against a 32 MiB bound "+
			"(samples=%d peak=%d blind=%q)", res.samples, res.peak, res.blind)
	}
	if res.peak < 32<<20 {
		t.Errorf("the probe reports a peak of %d bytes, under the bound it says it passed: a verdict "+
			"whose own number contradicts it", res.peak)
	}
}

func TestTheProbeLeavesAChildUnderItsBoundAlone(t *testing.T) {
	cmd, _ := rssHelper(t, "8", nil)
	stop := make(chan struct{})
	done := watchRSS(cmd, 256<<20, 5*time.Millisecond, stop)
	// Let it finish on its own, then stop the probe: a probe that killed a
	// child with room to spare would be bounding something other than memory.
	go func() { _ = cmd.Wait(); close(stop) }()
	res := <-done
	if res.killed {
		t.Fatalf("the probe killed a child that stayed under its bound (peak=%d)", res.peak)
	}
	if res.samples == 0 {
		t.Fatalf("the probe never sampled, so this test proves nothing: %s", res.blind)
	}
}

func TestABoundNobodyChoseIsNotProbed(t *testing.T) {
	cmd, _ := rssHelper(t, "8", nil)
	done := watchRSS(cmd, 0, 5*time.Millisecond, make(chan struct{}))
	_ = cmd.Wait()
	if res := <-done; res.samples != 0 || res.killed {
		t.Errorf("bound 0 means nobody chose, and nothing should be sampled: %+v", res)
	}
}

// TestRSSProbeHelper is not a test: it is the child the tests above spawn.
func TestRSSProbeHelper(t *testing.T) {
	mib := os.Getenv("ITERION_RSS_HELPER_MIB")
	if mib == "" {
		t.Skip("not the helper")
	}
	n, err := strconv.Atoi(mib)
	if err != nil {
		os.Exit(2)
	}
	// The bound the premise test names, applied by the child to itself: ROOM
	// above the data segment it already holds.
	if room := os.Getenv("ITERION_RLIMIT_DATA_ROOM_MIB"); room != "" {
		r, err := strconv.Atoi(room)
		if err != nil {
			os.Exit(7)
		}
		base, err := ownDataSegment()
		if err != nil {
			os.Exit(6)
		}
		lim := base + uint64(r)<<20
		if err := syscall.Setrlimit(syscall.RLIMIT_DATA, &syscall.Rlimit{Cur: lim, Max: lim}); err != nil {
			os.Exit(5)
		}
	}
	// MAP_SHARED|MAP_ANONYMOUS is the spelling RLIMIT_DATA does not charge.
	buf, err := syscall.Mmap(-1, 0, n<<20, syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_SHARED|syscall.MAP_ANON)
	if err != nil {
		os.Exit(3)
	}
	for i := 0; i < len(buf); i += 4096 {
		buf[i] = 1
	}
	time.Sleep(2 * time.Second)
	if err := syscall.Munmap(buf); err != nil {
		os.Exit(4)
	}
	os.Exit(0)
}

// rssHelper spawns the helper above in its own process group, which the probe
// needs to be able to kill the whole child rather than one of its threads.
// ownDataSegment is this process's VmData — mm->data_vm, the counter
// RLIMIT_DATA is compared with — in bytes.
func ownDataSegment() (uint64, error) {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok || name != "VmData" {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 1 {
			break
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0, err
		}
		return kb << 10, nil
	}
	return 0, errors.New("no VmData line in /proc/self/status")
}

func rssHelper(t *testing.T, mib string, env map[string]string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRSSProbeHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "ITERION_RSS_HELPER_MIB="+mib)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// The limit is set by the HELPER on itself: syscall.SysProcAttr carries no
	// Rlimits field on this platform, and a child that lowers its own bound
	// before allocating is the same guarantee.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The helper's stderr is the runtime's own account of a death, which an
	// exit status alone does not carry.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the helper: %v", err)
	}
	return cmd, &stderr
}

// The verdict must carry the probe's own numbers, not a shrug: an operator
// reading "memory_exceeded" needs the peak the parent measured to compare it
// with the bound it set.
func TestTheProbesVerdictNamesWhatItMeasured(t *testing.T) {
	resp, err := deathToRefusal(deathEvidence{
		limits:    Limits{Memory: 256 << 20, CPU: 10 * time.Second},
		signalled: true, signal: 9,
		probe: probeResult{killed: true, peak: 300 << 20, base: 40 << 20, samples: 4},
	})
	if err != nil {
		t.Fatalf("want a refusal, got %v", err)
	}
	for _, want := range []string{"314572800", "268435456", "41943040"} {
		if !strings.Contains(resp.Message, want) {
			t.Errorf("the message does not name %s: %s", want, resp.Message)
		}
	}
	if !strings.Contains(resp.Message, "MAP_SHARED") {
		t.Errorf("the message does not say WHICH growth it bounds: %s", resp.Message)
	}
}

// A probe that never sampled must say so where the death is otherwise
// unexplained: "the probe saw nothing" and "the probe could not look" are the
// same silence, and only one of them is a fact about the script.
func TestABlindProbeIsNamedOnAnUnexplainedKill(t *testing.T) {
	resp, err := deathToRefusal(deathEvidence{
		limits:    Limits{Memory: 256 << 20, CPU: 10 * time.Second},
		signalled: true, signal: 9,
		probe: probeResult{samples: 0, blind: "open /proc/42/status: no such file"},
	})
	if err != nil {
		t.Fatalf("want a refusal, got %v", err)
	}
	if !strings.Contains(resp.Message, "never sampled") {
		t.Errorf("a blind probe is not named: %s", resp.Message)
	}
}

// A refused OOM mark must reach the caller on a SUCCESSFUL run. The child's
// stderr is read only when it dies without a document, so a refusal written
// only there was lost on every run that returned a value — and a deployment
// that always refuses the write was never told.
func TestARefusedOOMMarkIsReportedOnASuccessfulRun(t *testing.T) {
	t.Parallel()
	var (
		mu    sync.Mutex
		warns []string
	)
	h := testHost(t)
	h.markVictim = func(int) error { return errors.New("permission denied") }
	h.OnWarn = func(msg string) {
		mu.Lock()
		defer mu.Unlock()
		warns = append(warns, msg)
	}
	resp, err := h.Run(context.Background(), "1+1", DefaultLimits())
	if err != nil {
		t.Fatalf("the run failed: %v", err)
	}
	if resp.Refusal != "" || resp.Value != "2" {
		t.Fatalf("the run did not succeed, so this proves nothing about the success path: %+v", resp)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(warns) != 1 {
		t.Fatalf("got %d warnings, want exactly one: %q", len(warns), warns)
	}
	if !strings.Contains(warns[0], "permission denied") || !strings.Contains(warns[0], "OOM victim") {
		t.Errorf("the warning does not carry the refusal and what it costs: %q", warns[0])
	}
}
