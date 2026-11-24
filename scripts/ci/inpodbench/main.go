// Command inpodbench reports what a codemode child actually confines itself
// to under the posture it is GIVEN, whether that confinement REFUSES, and
// whether the bounds bite there.
//
// WHY IT IS NOT THE UNIT SUITE. pkg/scripthost's own suite does not go green
// without Landlock — it fails loudly, and says so in its own words ("It does
// not skip […] must say so rather than report green"), skipping only when a
// lane sets ITERION_CODEMODE_ALLOW_NO_LANDLOCK=1 to go on record. That guard is
// sound and this binary does not replace it. What the suite cannot do is judge
// the SHIPPED image under the DEPLOYMENT's posture: it compiles on the host,
// runs on the host's kernel, and knows nothing of the pod's securityContext or
// the container runtime's seccomp profile — and in the default cloud posture
// nothing else stands between a script and the pod
// (`ITERION_SANDBOX_OVERRIDE: none`, "the pod is the isolation boundary"). A
// container profile can refuse the syscall the confinement is built on, and
// only something running inside that container can find out.
//
// ITS EXIT CODE IS A CONTRACT, not a boolean, because a caller that treats
// "non-zero" as "the bound bit" accepts a broken deployment as proof. A mutant
// run — one that denies the syscall the confinement is built on and must
// therefore be refused — is only honoured by exitPostureRefused. Any other
// failure (no image, no child binary, a wrong mode on this file, a flaky leg)
// lands on a different code, and the member driving this binary must reject
// it: otherwise the one thing proving the bench can redden is satisfied by
// every way it can break. Measured: with a 0700 build under a hardened umask
// the mutant arm failed with 126 and the member called it a success.
//
// It drives the real caller: scripthost.Host.Run spawns `iterion
// __script-host`, exactly as the model's codemode tool does. The only thing it
// replaces is the model.
//
// WHAT THIS BINARY DELIBERATELY DOES NOT MEASURE. Whether the child inherited
// the parent's environment cannot be observed from here: Confine calls
// prctl(PR_SET_DUMPABLE, 0), which hands /proc/<pid>/environ to root, so a
// same-uid reader gets EACCES whatever the child holds — and a leg that reads
// a failure as an absence is blind by construction. Measured: mutating the
// real constructor to `cmd.Env = os.Environ()` left such a leg green 8 runs
// out of 8, and an instrumented run showed 99% of its "observations" were
// refused reads. The guarantee lives in pkg/scripthost's
// TestTheChildIsBuiltWithNoEnvironment, which asserts on the constructor
// itself with real credential names in the environment.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/bits"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/SocialGouv/iterion/pkg/scripthost"
)

// The exit contract. Every caller branches on these and nothing else.
const (
	exitEveryLegHeld = 0
	exitLegFailed    = 1
	exitUsage        = 2
	// exitPostureRefused: a child could not confine itself, so no leg ran.
	// This is the ONLY code a mutant run may be credited for.
	exitPostureRefused = 3
	// exitWrongPosture: this process is not running under the posture the
	// caller required, so nothing it measured would be about that posture.
	exitWrongPosture = 4
	// exitPreconditionRefused: a leg could not run at all — this container is
	// too small for it, say — so nothing was measured about the deployment.
	// It used to arrive as "a leg failed", a red verdict about a deployment
	// that had not been tested.
	exitPreconditionRefused = 6
	// exitPostureUnchecked: -allow-any-posture was passed, so every verdict is
	// about this process and not about a deployment. It used to answer 0 from
	// a --privileged uid-0 container, which is the container round 3 was
	// written to refuse.
	exitPostureUnchecked = 7
	// exitUnconfined: -allow-without-landlock was passed and the kernel has no
	// Landlock, so a child ran with an unrestricted filesystem and
	// unrestricted sockets. Someone accepted that on purpose; it is never
	// success, because every leg below it would then measure the CPU and
	// memory bounds of an unconfined program.
	exitUnconfined = 5
)

// spinner is a CPU-bound program that runs long enough to be OBSERVED. A
// script cannot sleep — the host gives it no timers — so time passes here only
// by spending it.
const spinner = `let n=0; for(let i=0;i<6000000;i++){ n+=i%7 } n`

// spinnerBudget is the CPU bound the sibling leg gives its spinners. It is
// explicit and wide because the first version used the 10s default and the
// spinner spent ~11s of it, so the siblings died of their own limit while the
// leg blamed the bombs.
//
// There is deliberately NO assertion on how much of it a spinner spends. One
// was tried and was itself the flake it meant to prevent: it compared WALL
// time against a CPU bound, so under contention the same setting passed at
// 14.9s and failed at 17.0s with a healthy deployment underneath (measured at
// --cpus 0.5, which is the runner's request in values-prod.yaml). The case it
// anticipated needs no guess: a sibling that dies of its own bound returns
// cpu_time_exceeded, never reaches the completed count, and reddens this leg
// with the exact diagnosis.
const spinnerBudget = time.Minute

// bombMemory is the package's own default, and it may not be shrunk to make
// the leg cheaper: the leg proves the bound a deployment RUNS, which is the
// default. (The bound is room above the child's own data segment, #3383; a
// bomb at the 64 MiB floor dies with the runtime's "out of memory" too, but
// that is not the number production runs.)
//
// So the leg adapts to the container instead of to the bound: three
// concurrent bombs at this size need ~768 MiB, and under a tighter cgroup the
// kernel kills the host process first, which arrives as script_host_died and
// makes the leg blame the bombs for what the deployment did (measured at
// -m 512m and -m 256m). enoughMemoryForBombs refuses to conclude there.
const bombMemory = 256 << 20

// enoughMemoryForBombs reports whether this container can hold the leg's
// concurrent bombs, reading the cgroup rather than trusting the flags. An
// unreadable or unlimited value is not an obstacle: "max" is what a host
// without a limit says.
// postureUnchecked records that NO verdict here is about a deployment.
var postureUnchecked bool

// verdict replaces any exit code with exitPostureUnchecked when the posture was
// never checked. Gating only the SUCCESS exit was the same one-of-N-sites
// shape as every other finding here: with -allow-any-posture and a broken
// child the bench answered 1, "the deployment could not run a child at all" —
// a red verdict about a deployment from a run that had announced it was about
// nothing.
// EVERY os.Exit of a measuring path goes through this. Wrapping two of five
// terminal exits in the posture switch and leaving three bare was the FIFTH
// occurrence of this file's recurring defect — a guard applied to some members
// of a class — and it was inside the fix written to close that class. A new
// terminal exit here is a new member: wrap it, or the register's claim that
// exit 7 "replaces EVERY terminal code" becomes false again.
func verdict(code int) int {
	if postureUnchecked {
		fmt.Println("VERDICT: …and the posture was never checked, so this says nothing about a deployment")
		return exitPostureUnchecked
	}
	return code
}

// errPreconditionRefused marks a leg that could not run, as opposed to one
// that ran and failed. The two must not share an exit code.
var errPreconditionRefused = errors.New("precondition refused")

func enoughMemoryForBombs(bombs int) error {
	need := int64(bombs) * bombMemory * 3 / 2 // the bombs, plus their runtimes
	for _, path := range []string{
		"/sys/fs/cgroup/memory.max",
		"/sys/fs/cgroup/memory/memory.limit_in_bytes",
	} {
		raw := strings.TrimSpace(string(readFile(path)))
		if raw == "" || raw == "max" {
			continue
		}
		limit, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || limit <= 0 {
			continue
		}
		if limit < need {
			return fmt.Errorf("%w: this container's memory limit is %d MiB and %d concurrent %d MiB "+
				"bombs need about %d MiB, so the cgroup would kill the host process before any bound "+
				"bit and the refusal would arrive as %s with no bound named",
				errPreconditionRefused, limit>>20, bombs, bombMemory>>20, need>>20, scripthost.RefusalHostDied)
		}
	}
	return nil
}

const memoryBombSource = `const a=[]; for(;;){ a.push(new Array(1<<16).fill(7)) }`

// leg is one measurement: a program, and the refusal its bound owes.
type leg struct {
	name string
	src  string
	// want is the refusal code this leg must produce, "" for a value.
	want   string
	limits scripthost.Limits
}

func main() {
	binary := flag.String("binary", "", "path to the iterion binary that hosts the child (required)")
	allowWithout := flag.Bool("allow-without-landlock", false,
		"accept a kernel with no Landlock — reports the posture and exits 5 instead of refusing")
	// A STRING, parsed base 10 here: flag.Int is base 0, so `-require-uid 010`
	// silently meant 8 while the container ran uid 10.
	printProbes := flag.Bool("print-probe-set", false,
		"print the syscalls the posture check requires a runtime default to refuse, one per line, "+
			"and exit — the mutant profiles are generated from this so the two cannot drift")
	anyPosture := flag.Bool("allow-any-posture", false,
		"run without checking the posture at all, and say so in the output")
	requireUID := flag.String("require-uid", "",
		"refuse to measure unless this process runs as this uid, has a seccomp filter loaded, "+
			"holds no capability in its effective or bounding set, and has no_new_privs set "+
			"(empty disables the check)")
	// The supplementary groups a container runtime derives from the IMAGE are
	// not something the chart decides, so a pod whose image ships them can
	// only be measured if the operator names them. Empty keeps the strict
	// answer: the group `-require-uid` names, and nothing else.
	expectGroups := flag.String("expect-groups", "",
		"comma-separated supplementary group ids this deployment really gives (the image's own "+
			"groups included); observed and declared must match EXACTLY, in both directions. "+
			"Empty means only the group -require-uid names")
	flag.Parse()

	var declaredGroups []int
	if *expectGroups != "" {
		for _, raw := range strings.Split(*expectGroups, ",") {
			g, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil || g < 0 {
				fmt.Fprintf(os.Stderr, "inpodbench: -expect-groups %q: %q is not a group id\n", *expectGroups, raw)
				os.Exit(exitUsage)
			}
			declaredGroups = append(declaredGroups, g)
		}
	}

	// Strengthening the probe set broke the mutants twice: their profiles
	// denied one syscall, then two, while the set grew to five, and each time
	// the posture check refused them with exit 4 — correctly, and uselessly.
	// So the set has ONE source and the scripts read it from here.
	if *printProbes {
		fmt.Println("add_key")
		for _, p := range deniedBySeccompByDefault {
			fmt.Println(p.name)
		}
		return
	}

	// The escape probe is this binary confining ITSELF and then trying to get
	// out. It branches before anything else because it is not a bench run.
	if os.Getenv(escapeProbeEnv) != "" {
		beTheEscapeProbe()
		return
	}

	if *binary == "" {
		fmt.Fprintln(os.Stderr, "inpodbench: -binary is required: the child IS that binary")
		os.Exit(exitUsage)
	}

	// The containment check comes FIRST and is a chokepoint every arm of the
	// bench traverses. Printing the posture and concluding anyway was the
	// defect: a --privileged container with uid 0, no seccomp filter and every
	// capability reported "legs held" and exit 0, while the evidence file
	// recorded a posture nobody read.
	// The parent-under-test of the drain leg re-executes THIS binary, and it
	// branches BEFORE the posture check on purpose: it runs in the same
	// container as the process that already established the posture, so
	// re-checking there would only add a flag to forget. One was forgotten
	// once already — round 2 found -allow-without-landlock dropped on this
	// re-exec — and this ordering removes the class rather than the instance.
	if os.Getenv(drainChildEnv) != "" {
		beTheParentUnderTest(scripthost.Host{Binary: *binary, AllowWithoutLandlock: *allowWithout})
		return
	}

	// An UNCHECKED posture may not produce a success. Leaving -require-uid
	// empty used to run every leg and exit 0, while the page said the bench
	// "refuses to measure unless …": a caller who dropped the flag got a
	// green about nothing.
	switch {
	case *requireUID != "":
		wantUID, err := strconv.ParseInt(*requireUID, 10, 32)
		if err != nil || wantUID < 0 {
			fmt.Fprintf(os.Stderr, "inpodbench: -require-uid %q is not a uid\n", *requireUID)
			os.Exit(exitUsage)
		}
		if err := requirePosture(int(wantUID), declaredGroups); err != nil {
			fmt.Printf("VERDICT: this process is not under the posture it was asked to measure: %v\n", err)
			// Through verdict() like every other measuring exit. It is a no-op
			// on this branch — reaching it means -require-uid was given, so
			// the posture WAS checked — and a reader should not have to prove
			// that to trust the exit contract.
			os.Exit(verdict(exitWrongPosture))
		}
	case *anyPosture:
		fmt.Println("NOTE the posture is NOT checked (-allow-any-posture): every verdict below is " +
			"about this process, not about the deployment's posture, and this run cannot answer 0")
		postureUnchecked = true
	default:
		fmt.Fprintln(os.Stderr, "inpodbench: pass -require-uid <n> so the posture is checked, or "+
			"-allow-any-posture to state that you accept a verdict about nothing")
		os.Exit(exitUsage)
	}

	host := scripthost.Host{Binary: *binary, AllowWithoutLandlock: *allowWithout}

	fmt.Printf("SELF %s\n", describeSelf(*binary))

	// The posture leg runs first and alone, and it JUDGES what it prints.
	// Checking only that the child answered was the defect that let
	// -allow-without-landlock report "legs held" for a child with an
	// unrestricted filesystem — the exact outcome this file exists to prevent.
	posture, err := host.Run(context.Background(), "1+1", scripthost.Limits{})
	report("posture", posture, err)
	switch {
	case err != nil:
		// A host error is a broken deployment, not a refused confinement: it
		// must not be mistaken for the mutant's expected outcome.
		fmt.Printf("VERDICT: the deployment could not run a child at all: %v\n", err)
		os.Exit(verdict(exitLegFailed))
	case !posture.OK():
		fmt.Printf("VERDICT: this deployment cannot host a confined codemode child: %s %s\n",
			posture.Refusal, posture.Message)
		os.Exit(verdict(exitPostureRefused))
	case posture.Confinement == nil:
		fmt.Println("VERDICT: the child reported no confinement at all, so nothing below it is about a confined child")
		os.Exit(verdict(exitLegFailed))
	case posture.Confinement.LandlockABI == 0:
		// Reachable only with -allow-without-landlock: without it the child
		// refuses and lands on exitPostureRefused above.
		fmt.Println("VERDICT: the child ran with NO Landlock domain — an unrestricted filesystem. " +
			"-allow-without-landlock accepted that; it is not a pass.")
		os.Exit(verdict(exitUnconfined))
	case !posture.Confinement.AllThreads:
		fmt.Println("VERDICT: the bounds covered ONE thread, not the process. That is not what codemode promises.")
		os.Exit(verdict(exitLegFailed))
	case !posture.Confinement.SocketsDenied:
		// The IMAGE's child, not this bench's: leg 5 below proves the bench's
		// own Confine, which is HEAD. A deployed binary that predates the
		// socket wall reports false here, and a posture leg that let it pass
		// would call a child with an open network confined.
		fmt.Println("VERDICT: the image's child does not deny itself socket creation (sockets_denied=false): " +
			"it predates the network wall, so a compromised child of this deployment reaches the network.")
		os.Exit(verdict(exitLegFailed))
	}

	legs := []leg{{
		name:   "a memory bomb is killed by the memory bound",
		src:    memoryBombSource,
		want:   scripthost.RefusalMemory,
		limits: scripthost.Limits{Memory: bombMemory},
	}, {
		name: "a native regex bomb is killed by the CPU bound",
		// The BACKREFERENCE is what makes this a bomb: goja compiles a pattern
		// without one to RE2, which is linear, and the leg then measures
		// nothing. Same form as TestANativeRegexBombIsKilledByTheCPUBound.
		src:  `/(a+)+\1b$/.test('a'.repeat(40))`,
		want: scripthost.RefusalCPUTime,
		// A CPU bound this low is the point: the bomb must die on the
		// kernel's clock, not on the parent's wall backstop. A
		// wall_clock_exceeded here would mean the kernel bound did not bite.
		limits: scripthost.Limits{CPU: time.Second, Wall: time.Minute, Memory: 256 << 20},
	}, {
		name: "a benign program still completes after the bombs",
		src:  `[1,2,3].map(x => x*2).join(",")`,
	}}

	// The posture leg is counted: "6/6" for seven measurements was a quiet
	// off-by-one in the one number a reader takes away.
	held, total := 1, 1
	for _, l := range legs {
		total++
		resp, err := host.Run(context.Background(), l.src, l.limits)
		report(l.name, resp, err)
		switch {
		case err != nil:
			fmt.Printf("  FAIL %s: host error: %v\n", l.name, err)
		case resp.Refusal != l.want:
			fmt.Printf("  FAIL %s: refusal %q, want %q (%s)\n",
				l.name, resp.Refusal, l.want, resp.Message)
		default:
			held++
		}
	}

	refused := 0
	for _, extra := range []func(scripthost.Host) error{
		aCompromisedChildCannotEscape,
		aBombDoesNotTakeDownASiblingRun,
		noDescendantOutlivesAKilledParent,
	} {
		total++
		err := extra(host)
		switch {
		case err == nil:
			held++
		case errors.Is(err, errPreconditionRefused):
			fmt.Printf("  REFUSED %v\n", err)
			refused++
		default:
			fmt.Printf("  FAIL %v\n", err)
		}
	}

	if refused > 0 && held+refused == total {
		fmt.Printf("VERDICT: %d/%d legs could not run here, and nothing else failed\n", refused, total)
		os.Exit(verdict(exitPreconditionRefused))
	}

	if held != total {
		fmt.Printf("VERDICT: %d/%d legs failed\n", total-held, total)
		os.Exit(verdict(exitLegFailed))
	}
	fmt.Printf("VERDICT: %d/%d legs held\n", total, total)
	os.Exit(verdict(exitEveryLegHeld))
}

// report prints one measurement, posture included, as a line a gate log keeps.
func report(name string, resp scripthost.Response, err error) {
	c, _ := json.Marshal(resp.Confinement)
	fmt.Printf("LEG %-50s refusal=%-22q confinement=%s err=%v\n",
		name, resp.Refusal, c, err)
}

// ---------------------------------------------------------------------------
// The escape suite: a COMPROMISED child, not a sandboxed script.
//
// Every other leg measures a bound on a well-behaved program. This one asks
// the question #3155 asks: if the child's own code were hostile, does the
// domain REFUSE? Reading Applied.LandlockABI only says a ruleset was
// INSTALLED. A number is not a refusal.
//
// The probe is this binary: it confines itself with the real
// scripthost.Confine, then tries to open a file, connect to a socket and exec
// a program. Each attempt is made TWICE — once before Confine and once after —
// because an attempt that fails both times proves nothing about the
// confinement, only that the environment never allowed it. The before/after
// pair is the whole oracle.
// ---------------------------------------------------------------------------

const (
	escapeProbeEnv   = "INPODBENCH_ESCAPE_PROBE"
	escapeFileEnv    = "INPODBENCH_ESCAPE_FILE"
	escapeConnectEnv = "INPODBENCH_ESCAPE_CONNECT"
	escapeUDPEnv     = "INPODBENCH_ESCAPE_UDP"
	escapeExecEnv    = "INPODBENCH_ESCAPE_EXEC"
)

// attempt is one escape attempt, before and after the confinement. The ERRNO
// is carried, not just the text, because "it failed" is not the measurement:
// a mutant that installed no Landlock domain at all and lowered RLIMIT_NOFILE
// to zero instead made all three attempts fail with EMFILE, and a leg that
// only asked whether they failed printed "too many open files" and passed.
type attempt struct {
	Before      string `json:"before"` // "" on success, else the error text
	BeforeErrno int    `json:"before_errno"`
	After       string `json:"after"`
	AfterErrno  int    `json:"after_errno"`
}

// heldBy reports why this attempt does NOT witness the confinement, or nil.
//
// Landlock refuses an open, an exec and a connect with EACCES — measured on
// ABI 4, connect included. Any other errno means something else stopped the
// attempt — a deleted file, a closed port, an exhausted descriptor table — and
// the leg must say so rather than bank it.
//
// Residual limit, stated rather than papered over: this ORACLE cannot
// ATTRIBUTE an EACCES to Landlock. A deployment that stubbed the domain and
// installed some other EACCES mechanism (a seccomp filter on openat, connect
// and execve) would satisfy it. No environment produces that, and the
// before/after pair closes every case that does occur.
func (a attempt) heldBy(what string) error {
	switch {
	case a.Before != "":
		return fmt.Errorf("%s was already impossible BEFORE the confinement (%s), so its failure "+
			"afterwards proves nothing about Landlock", what, a.Before)
	case a.After == "":
		return fmt.Errorf("%s SUCCEEDED inside the confinement: the domain does not refuse it", what)
	case a.AfterErrno != int(syscall.EACCES):
		return fmt.Errorf("%s failed inside the confinement with errno %d (%s), not EACCES (%d): "+
			"something OTHER than the Landlock domain stopped it, so this is not a witness",
			what, a.AfterErrno, a.After, int(syscall.EACCES))
	}
	return nil
}

// errnoOf digs the syscall errno out of whatever wrapper produced it: a
// PathError from open, an OpError from dial, an exec.Error from fork/exec.
// 0 when there is none, which heldBy treats as "not EACCES".
func errnoOf(err error) int {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return int(errno)
	}
	return 0
}

type escapeReport struct {
	Applied scripthost.Applied `json:"applied"`
	Open    attempt            `json:"open"`
	Connect attempt            `json:"connect"`
	SendUDP attempt            `json:"send_udp"`
	IOURing attempt            `json:"io_uring"`
	Exec    attempt            `json:"exec"`
	Fatal   string             `json:"fatal,omitempty"`
}

// refusedAfter is the weaker oracle for io_uring, and only for it: a runtime's
// own seccomp profile may refuse io_uring_setup BEFORE the confinement (docker's
// and containerd's defaults do, with EPERM), so the before/after pair cannot be
// required. What is required is that the confined child cannot have it — by
// the child's filter (EACCES) or by the runtime's (EPERM).
func (a attempt) refusedAfter(what string) error {
	switch {
	case a.After == "":
		return fmt.Errorf("%s SUCCEEDED inside the confinement", what)
	case a.AfterErrno != int(syscall.EACCES) && a.AfterErrno != int(syscall.EPERM):
		return fmt.Errorf("%s failed inside the confinement with errno %d (%s), not a permission "+
			"refusal: something else stopped it, so this is not a witness", what, a.AfterErrno, a.After)
	}
	return nil
}

func aCompromisedChildCannotEscape(host scripthost.Host) error {
	const name = "a compromised child cannot open, connect or exec"

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("%s: cannot find its own binary: %w", name, err)
	}

	// A file the probe must be able to read BEFORE and not after. It lives in
	// a directory this process owns and the framework removes.
	dir, err := os.MkdirTemp("", "inpodbench-escape")
	if err != nil {
		return fmt.Errorf("%s: cannot make its own scratch dir: %w", name, err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	target := dir + "/readable"
	if err := os.WriteFile(target, []byte("reachable"), 0o600); err != nil {
		return fmt.Errorf("%s: cannot write its own probe file: %w", name, err)
	}

	// A listener in this process: connecting to it is a real connect(2), and
	// it cannot be mistaken for a network that was never reachable.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("%s: cannot listen for the connect attempt: %w", name, err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	// The UDP target: no Landlock right has ever covered UDP, so this is the
	// attempt that tells the socket wall from the TCP one on every ABI.
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("%s: cannot listen for the UDP attempt: %w", name, err)
	}
	defer func() { _ = udp.Close() }()

	// Something executable that exists in THIS image. A missing one would make
	// the exec attempt fail on both sides and prove nothing, which heldBy
	// refuses rather than passes.
	execTarget := ""
	for _, candidate := range []string{"/bin/true", "/usr/bin/true", "/bin/sh"} {
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			execTarget = candidate
			break
		}
	}
	if execTarget == "" {
		return fmt.Errorf("%s: this image has none of /bin/true, /usr/bin/true, /bin/sh, so the exec "+
			"attempt would fail for want of a target and prove nothing", name)
	}

	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(),
		escapeProbeEnv+"=1",
		escapeFileEnv+"="+target,
		escapeConnectEnv+"="+ln.Addr().String(),
		escapeUDPEnv+"="+udp.LocalAddr().String(),
		escapeExecEnv+"="+execTarget,
	)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("%s: the probe did not report: %w", name, err)
	}
	var rep escapeReport
	if err := json.Unmarshal(out, &rep); err != nil {
		return fmt.Errorf("%s: the probe's report is unreadable (%w): %s", name, err, out)
	}
	if rep.Fatal != "" {
		return fmt.Errorf("%s: the probe could not confine itself, so it never tried to escape: %s",
			name, rep.Fatal)
	}

	fmt.Printf("LEG %-50s abi=%d all_threads=%t sockets_denied=%t open=%s connect=%s udp=%s io_uring=%s exec=%s\n",
		name, rep.Applied.LandlockABI, rep.Applied.AllThreads, rep.Applied.SocketsDenied,
		shortErrno(rep.Open), shortErrno(rep.Connect), shortErrno(rep.SendUDP), shortErrno(rep.IOURing),
		shortErrno(rep.Exec))

	// The network attempts are required on EVERY ABI. They used to be exempt
	// below ABI 4, where Landlock has no TCP right — and on the production
	// nodes (ABI 2) a compromised child's connect SUCCEEDED. The socket wall
	// (scripthost's seccomp filter) does not depend on Landlock.
	var problems []string
	if !rep.Applied.SocketsDenied {
		problems = append(problems, "the confinement reports no socket wall (sockets_denied=false)")
	}
	for _, c := range []struct {
		what string
		a    attempt
	}{
		{"open", rep.Open},
		{"exec", rep.Exec},
		{"connect", rep.Connect},
		{"a UDP send", rep.SendUDP},
	} {
		if err := c.a.heldBy(c.what); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if err := rep.IOURing.refusedAfter("io_uring_setup"); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s: %s", name, strings.Join(problems, "; "))
	}
	return nil
}

// beTheEscapeProbe confines itself with the real primitive and then tries to
// get out, reporting every attempt from both sides of the confinement. It
// always exits 0: the report is the measurement, and the caller judges it.
func beTheEscapeProbe() {
	rep := escapeReport{}
	target, addr, execTarget := os.Getenv(escapeFileEnv), os.Getenv(escapeConnectEnv), os.Getenv(escapeExecEnv)
	udpAddr := os.Getenv(escapeUDPEnv)

	tryOpen := func() error {
		f, err := os.Open(target)
		if err == nil {
			_ = f.Close()
		}
		return err
	}
	tryConnect := func() error {
		c, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err == nil {
			_ = c.Close()
		}
		return err
	}
	trySendUDP := func() error {
		c, err := net.Dial("udp", udpAddr)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		_, err = c.Write([]byte("escape"))
		return err
	}
	// io_uring can create a socket without socket(2) (IORING_OP_SOCKET).
	tryIOURing := func() error {
		var params [120]byte // struct io_uring_params
		fd, _, errno := syscall.Syscall(unix.SYS_IO_URING_SETUP, 1, uintptr(unsafe.Pointer(&params[0])), 0)
		if errno != 0 {
			return errno
		}
		_ = syscall.Close(int(fd))
		return nil
	}
	tryExec := func() error { return exec.Command(execTarget, "-c", ":").Run() }

	rep.Open.Before, rep.Open.BeforeErrno = describe(tryOpen())
	rep.Connect.Before, rep.Connect.BeforeErrno = describe(tryConnect())
	rep.SendUDP.Before, rep.SendUDP.BeforeErrno = describe(trySendUDP())
	rep.IOURing.Before, rep.IOURing.BeforeErrno = describe(tryIOURing())
	rep.Exec.Before, rep.Exec.BeforeErrno = describe(tryExec())

	applied, err := scripthost.Confine(scripthost.Limits{
		// The ceiling, not the defaults: a probe killed by its own CPU bound
		// mid-attempt would look like a refusal.
		CPU: scripthost.Ceiling().CPU, Wall: scripthost.Ceiling().Wall, Memory: 256 << 20,
	})
	if err != nil {
		rep.Fatal = err.Error()
		emit(rep)
		return
	}
	rep.Applied = applied

	rep.Open.After, rep.Open.AfterErrno = describe(tryOpen())
	rep.Connect.After, rep.Connect.AfterErrno = describe(tryConnect())
	rep.SendUDP.After, rep.SendUDP.AfterErrno = describe(trySendUDP())
	rep.IOURing.After, rep.IOURing.AfterErrno = describe(tryIOURing())
	rep.Exec.After, rep.Exec.AfterErrno = describe(tryExec())
	emit(rep)
}

// describe turns an attempt's outcome into the pair the report carries.
func describe(err error) (string, int) {
	if err == nil {
		return "", 0
	}
	return err.Error(), errnoOf(err)
}

// emit is the escape PROBE's own exit path, not a measuring one: its code is
// read by the leg's cmd.Output() as "the probe did not report", never by a
// gate, so it does not go through verdict(). The same holds for
// beTheParentUnderTest, whose handshake the leg reads instead of its code.
func emit(rep escapeReport) {
	b, err := json.Marshal(rep)
	if err != nil {
		// Nothing else can be said: stdout is the only channel and it is the
		// report. An empty stdout makes the caller refuse, which is correct.
		fmt.Fprintln(os.Stderr, "inpodbench: the probe cannot encode its own report:", err)
		os.Exit(exitLegFailed)
	}
	fmt.Println(string(b))
}

// shortErrno names the outcome AND its errno on one line: "it failed" was the
// reading that let EMFILE pass for a Landlock refusal, so the log says which.
func shortErrno(a attempt) string {
	if a.After == "" {
		return "SUCCEEDED"
	}
	text := a.After
	if i := strings.LastIndex(text, ": "); i > 0 && i+2 < len(text) {
		text = text[i+2:]
	}
	return fmt.Sprintf("%q/errno=%d", text, a.AfterErrno)
}

// ---------------------------------------------------------------------------

// aBombDoesNotTakeDownASiblingRun is the leg that makes the pod the unit. In
// the default cloud posture the runner pod holds several runs at once, so a
// bound that kills the POD instead of the child would turn one bad script into
// an outage for everything scheduled beside it.
//
// IT ASSERTS A BOMB AND A SIBLING ALIVE AT THE SAME INSTANT, and the two
// weaker forms it replaced both died to a mutant. Comparing timestamps taken
// around host.Run was green under a global lock, because those windows include
// the wait in queue. Counting live children was green under a scheduler that
// admitted the three siblings together and then the three bombs together —
// either group alone reaches two, so "two children" never meant "a bomb
// BESIDE a run".
//
// The discriminator comes from the producer: command() puts Host.Binary in the
// child's argv, and /proc/<pid>/cmdline is what childrenOf already reads. So
// the bombs run through a second Host whose Binary is a symlink to the same
// file, and the sampler classifies each live child by its argv[0]. The symlink
// need not be executable where it sits: execve resolves it to the image layer,
// which is why this works in the shipped image where /tmp is noexec.
func aBombDoesNotTakeDownASiblingRun(host scripthost.Host) error {
	const bombs, siblings = 3, 3
	const name = "a bomb does not take down a sibling run"

	if err := enoughMemoryForBombs(bombs); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}

	// The bombs' own view of the same binary, under a name the sampler can
	// tell apart from the siblings'.
	dir, err := os.MkdirTemp("", "inpodbench-kinds")
	if err != nil {
		return fmt.Errorf("%s: cannot make its own scratch dir: %w", name, err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	bombBinary := dir + "/iterion-bomb"
	if err := os.Symlink(host.Binary, bombBinary); err != nil {
		return fmt.Errorf("%s: cannot name the bombs' binary apart: %w", name, err)
	}
	bombHost := host
	bombHost.Binary = bombBinary

	type span struct {
		kind string
		resp scripthost.Response
		err  error
	}
	results := make(chan span, bombs+siblings)

	type sample struct{ peak, withBoth int }
	sampled := make(chan sample, 1)
	stopSampling := make(chan struct{})
	go func() {
		var out sample
		for {
			select {
			case <-stopSampling:
				sampled <- out
				return
			case <-time.After(5 * time.Millisecond):
				sib, bomb := childKinds(os.Getpid(), host.Binary, bombBinary)
				if sib+bomb > out.peak {
					out.peak = sib + bomb
				}
				if sib > 0 && bomb > 0 {
					out.withBoth++
				}
			}
		}
	}()

	// The siblings are started first and the bombs released after. This only
	// orders the SPAWNS — running.Done() runs before host.Run, so the wait
	// proves three goroutines were scheduled and nothing more. What proves
	// the groups actually met is the sampler below, not this.
	var running sync.WaitGroup
	running.Add(siblings)
	release := make(chan struct{})

	for range siblings {
		go func() {
			running.Done()
			resp, err := host.Run(context.Background(), spinner,
				scripthost.Limits{CPU: spinnerBudget, Wall: scripthost.Ceiling().Wall})
			results <- span{"sibling", resp, err}
		}()
	}
	running.Wait()
	close(release)

	for range bombs {
		go func() {
			<-release
			// The ceiling's wall bound, for the reason the siblings have an
			// explicit CPU one: on the DEFAULT 30s wall these died of
			// wall_clock_exceeded at --cpus 0.1 while the deployment was
			// healthy, which is the same flake class in the other group.
			resp, err := bombHost.Run(context.Background(), memoryBombSource,
				scripthost.Limits{Memory: bombMemory, Wall: scripthost.Ceiling().Wall})
			results <- span{"bomb", resp, err}
		}()
	}

	var bombsRefused, siblingsDone int
	var problems []string
	for range bombs + siblings {
		s := <-results
		switch {
		case s.err != nil:
			problems = append(problems, fmt.Sprintf("%s: host error: %v", s.kind, s.err))
		case s.kind == "bomb" && s.resp.Refusal == scripthost.RefusalMemory:
			bombsRefused++
		case s.kind == "sibling" && s.resp.OK():
			siblingsDone++
		default:
			problems = append(problems, fmt.Sprintf("%s: refusal %q (%s)",
				s.kind, s.resp.Refusal, s.resp.Message))
		}
	}
	close(stopSampling)
	seen := <-sampled

	fmt.Printf("LEG %-50s bombs_refused=%d/%d siblings_completed=%d/%d peak_children=%d samples_with_both_kinds=%d\n",
		name, bombsRefused, bombs, siblingsDone, siblings, seen.peak, seen.withBoth)

	switch {
	case bombsRefused != bombs || siblingsDone != siblings:
		return fmt.Errorf("%s: %d/%d bombs refused, %d/%d siblings completed: %v",
			name, bombsRefused, bombs, siblingsDone, siblings, problems)
	case seen.withBoth == 0:
		return fmt.Errorf("%s: no sample saw a bomb and a sibling alive at the same time (peak was %d "+
			"children, all of one kind), so this leg measured two QUEUES and not a bomb beside a run",
			name, seen.peak)
	}
	return nil
}

// childKinds counts the live script-host children of pid, split by the binary
// their argv names. The two kinds are the whole point: a count that cannot
// tell them apart is satisfied by either group alone.
func childKinds(pid int, siblingBinary, bombBinary string) (siblings, bombs int) {
	for _, child := range childrenOf(pid) {
		switch argv0(child) {
		case siblingBinary:
			siblings++
		case bombBinary:
			bombs++
		}
	}
	return siblings, bombs
}

// argv0 is the child's own view of which binary it is, NUL-separated in
// /proc/<pid>/cmdline.
func argv0(pid int) string {
	raw := readFile(procPath(pid, "cmdline"))
	if i := bytes.IndexByte(raw, 0); i >= 0 {
		return string(raw[:i])
	}
	return string(raw)
}

// noDescendantOutlivesAKilledParent is the drain leg. A pod is drained by
// SIGTERM and then SIGKILL, and a SIGKILLed parent runs no cleanup at all, so
// whatever reaps the child is the kernel's business and not the parent's.
//
// IT WITNESSES THE PROPERTY, NOT A MECHANISM, and the difference is measured.
// applyParentDeathSignal's own doc calls Pdeathsig a BACKSTOP rather than the
// orphan bound ("delivered when the forking THREAD exits, not the process"),
// and a mutation bench agrees: with that function turned into a no-op this leg
// stays green, because something else still reaps the descendant. That mutant
// is an ASSUMED SURVIVOR, named here rather than hidden — this leg proves no
// descendant outlives a killed parent, which is what a drain needs, and it
// proves nothing about which of the holders did it.
//
// Two earlier shapes of it could not redden at all, and both are fixed. The
// descendant now runs at the PLATFORM CEILING: on the default bounds it died
// of its own 10s RLIMIT_CPU inside a 30s window, so the window measured the
// descendant's own clock. And the wait is SHORT and fatal on expiry, because
// waiting long enough for any exit turns the assertion into a tautology.
func noDescendantOutlivesAKilledParent(host scripthost.Host) error {
	const name = "no descendant outlives a SIGKILLed parent"
	const drainWindow = 2 * time.Second

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("%s: cannot find its own binary: %w", name, err)
	}

	// Every posture flag travels to the re-exec. Dropping them made the
	// parent-under-test's child refuse on a kernel the outer process had been
	// told to accept, and the leg's verdict then depended on which of the two
	// raced — 7 greens and 1 red over 8 identical runs.
	args := []string{"-binary", host.Binary}
	if host.AllowWithoutLandlock {
		args = append(args, "-allow-without-landlock")
	}
	cmd := exec.Command(self, args...)
	cmd.Env = append(os.Environ(), drainChildEnv+"=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("%s: cannot read its parent-under-test: %w", name, err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: cannot start its parent-under-test: %w", name, err)
	}
	kill := func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}

	// A deadline on the handshake, because the parent-under-test can fail to
	// produce a child at all: with a missing -binary it spun on a bare
	// `for len(children)==0 {}` forever, burning ~1.7 cores, and the bench
	// neither passed nor failed — it stopped, with a green leg as its last
	// line.
	readyErr := make(chan error, 1)
	go func() {
		ready := make([]byte, len(readyLine))
		_, err := io.ReadFull(out, ready)
		if err == nil && string(ready) != readyLine {
			err = fmt.Errorf("announced %q, want %q", ready, readyLine)
		}
		readyErr <- err
	}()
	select {
	case err := <-readyErr:
		if err != nil {
			kill()
			return fmt.Errorf("%s: the parent-under-test never announced a child: %w", name, err)
		}
	case <-time.After(handshakeWindow):
		kill()
		return fmt.Errorf("%s: the parent-under-test did not announce a child within %s", name, handshakeWindow)
	}

	spawned := childrenOf(cmd.Process.Pid)
	if len(spawned) == 0 {
		kill()
		return fmt.Errorf("%s: no child is parented to the process under test, so there is nothing "+
			"to outlive it", name)
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		return fmt.Errorf("%s: cannot SIGKILL its parent-under-test: %w", name, err)
	}
	_ = cmd.Wait()

	// The kernel delivers the death signal; this waits for it rather than
	// assuming it already happened, and gives up rather than waiting out the
	// descendant's own bounds.
	deadline := time.Now().Add(drainWindow)
	survivors := spawned
	for time.Now().Before(deadline) && len(survivors) > 0 {
		time.Sleep(10 * time.Millisecond)
		survivors = stillAlive(spawned)
	}

	fmt.Printf("LEG %-50s spawned=%d survivors=%d window=%s\n",
		name, len(spawned), len(survivors), drainWindow)
	if len(survivors) > 0 {
		return fmt.Errorf("%s: %d descendant(s) outlived their SIGKILLed parent by more than %s: pids %v",
			name, len(survivors), drainWindow, survivors)
	}
	return nil
}

const (
	drainChildEnv   = "INPODBENCH_DRAIN_PARENT"
	readyLine       = "READY\n"
	handshakeWindow = 30 * time.Second
)

// beTheParentUnderTest holds exactly one child and waits to be killed. It
// announces the child on stdout so the outer bench kills a parent that HAS one
// — killing it earlier would prove nothing about a descendant. The child runs
// at the platform ceiling so that it cannot die of its own bound during the
// drain window and make the death signal look effective.
func beTheParentUnderTest(host scripthost.Host) {
	runErr := make(chan error, 1)
	go func() {
		ceiling := scripthost.Ceiling()
		_, err := host.Run(context.Background(), spinner,
			scripthost.Limits{CPU: ceiling.CPU, Wall: ceiling.Wall, Memory: 256 << 20})
		runErr <- err
	}()

	// A deadline and a short sleep: a bare spin here cost ~1.7 cores on a pod
	// that is meant to be running other people's work, and it never ended.
	deadline := time.Now().Add(handshakeWindow)
	for time.Now().Before(deadline) {
		if len(childrenOf(os.Getpid())) > 0 {
			fmt.Print(readyLine)
			_ = os.Stdout.Sync()
			<-make(chan struct{}) // the outer bench SIGKILLs this process
		}
		select {
		case err := <-runErr:
			fmt.Fprintf(os.Stderr,
				"inpodbench: the parent-under-test's child ended before it could be seen: %v\n", err)
			os.Exit(exitLegFailed)
		case <-time.After(10 * time.Millisecond):
		}
	}
	fmt.Fprintf(os.Stderr, "inpodbench: the parent-under-test saw no child within %s\n", handshakeWindow)
	os.Exit(exitLegFailed)
}

// childrenOf lists the script-host processes whose PARENT is pid, deduplicated.
//
// Parentage is the whole point. Selecting on the command line alone matched
// any neighbour whose argv merely mentioned the subcommand: a stranger both
// forged results and satisfied a leg's anti-vacuous guard — and in a runner
// pod, which is what this bench is for, the children of OTHER runs satisfy it
// permanently.
func childrenOf(pid int) []int {
	var out []int
	for _, entry := range pids() {
		if parentOf(entry) != pid {
			continue
		}
		if !strings.Contains(string(readFile(procPath(entry, "cmdline"))), scripthost.Subcommand) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// stillAlive returns the members of want that are still RUNNING.
//
// A zombie is not alive, and telling them apart is the whole of this function.
// kill(pid, 0) cannot: a reaped-but-unwaited process keeps its pid until its
// parent calls wait, and the parent here has just been SIGKILLed — so the
// descendant is reparented to PID 1, and whether the pid disappears depends on
// whether THAT process reaps. Measured: in a container whose entrypoint was
// overridden to `sleep`, which never waits, the descendant was dead within
// 10ms and its pid persisted indefinitely; the leg read that as a survivor and
// failed for the container's init rather than for the drain. The shipped image
// runs `tini`, which does reap, so the bench must not depend on either.
//
// /proc/<pid>/stat field 3 is the state letter. Z is a zombie: the process has
// exited and nothing it was doing continues.
func stillAlive(want []int) []int {
	var out []int
	for _, pid := range want {
		switch stateOf(pid) {
		case "", "Z", "X", "x":
			// gone, or exited and not yet reaped
		default:
			out = append(out, pid)
		}
	}
	return out
}

// stateOf reads the state letter from /proc/<pid>/stat, "" when the process is
// gone. The comm field can contain spaces and parentheses, so the scan starts
// after its LAST closing one.
func stateOf(pid int) string {
	stat := string(readFile(procPath(pid, "stat")))
	end := strings.LastIndexByte(stat, ')')
	if end < 0 || end+2 >= len(stat) {
		return ""
	}
	fields := strings.Fields(stat[end+2:])
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// parentOf reads field 4 of /proc/<pid>/stat, the ppid. The comm field can
// contain spaces and parentheses, so the scan starts after its LAST one.
func parentOf(pid int) int {
	stat := string(readFile(procPath(pid, "stat")))
	end := strings.LastIndexByte(stat, ')')
	if end < 0 || end+2 >= len(stat) {
		return -1
	}
	fields := strings.Fields(stat[end+2:])
	if len(fields) < 2 {
		return -1
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return -1
	}
	return ppid
}

func procPath(pid int, file string) string { return "/proc/" + strconv.Itoa(pid) + "/" + file }

func pids() []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		if pid, err := strconv.Atoi(e.Name()); err == nil {
			out = append(out, pid)
		}
	}
	return out
}

// readFile reads a /proc file, treating any failure as absence: a process that
// exits between the listing and the read is the normal case here. No leg may
// conclude from that absence alone — see the package comment.
func readFile(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}

// requirePosture refuses to measure unless this process is actually confined
// the way the caller said it would be. It reads the kernel's own answer, never
// the flags someone believes were passed: a flag an old daemon ignores, or one
// edited out of a `docker run` line, leaves no other trace.
//
// Seccomp: 2 is SECCOMP_MODE_FILTER. The twin reading is what makes it mean
// something — an Unconfined container reports 0 — and both are measured.
// groupSet renders a supplementary-group set in a stable order, so a message
// never depends on map iteration order (a flaky expectation is worse than a
// missing one).
func groupSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := strconv.Atoi(out[i])
		b, _ := strconv.Atoi(out[j])
		return a < b
	})
	return out
}

// checkGroups judges the `Groups:` field of /proc/self/status against the
// posture asked for. It is a pure function of that field so both directions
// have a witness that needs no process.
//
// With NOTHING declared, only a widening is refused: a group other than the
// one the chart names (fsGroup = runAsUser). The ABSENCE of that group widens
// nothing, and the first version of this check did not require it — docker's
// `--user uid:gid` puts the gid there today (measured on 29.10: `Groups:
// 1000`), but the docker arm must not start refusing its own posture on a
// runtime that does not.
//
// With a set DECLARED, it is the deployment's real answer, named by the
// operator for the case the chart does not decide: a container runtime adds
// the IMAGE's supplementary groups (`devbox` is in `sudo` and `users` in the
// runner image). Then the match is EXACT in both directions — a group beyond
// the set is a widening nobody stated, and a declared group that is absent
// means the declaration does not describe this pod.
func checkGroups(groupsField string, wantUID int, declared []int) error {
	want := map[string]bool{strconv.Itoa(wantUID): true}
	if len(declared) > 0 {
		want = map[string]bool{}
		for _, g := range declared {
			want[strconv.Itoa(g)] = true
		}
	}
	observed := map[string]bool{}
	for _, g := range strings.Fields(groupsField) {
		observed[g] = true
		if !want[g] {
			return fmt.Errorf("supplementary group %s (Groups: %s), want only %v: the chart sets "+
				"fsGroup to runAsUser and names no supplementalGroups, so a group beyond the "+
				"declared set is a widening the deployment did not state",
				g, groupsField, groupSet(want))
		}
	}
	if len(declared) == 0 {
		return nil
	}
	// In SORTED order: ranging over the map named a random absent group when
	// several were missing, and a message must not depend on map order.
	for _, g := range groupSet(want) {
		if !observed[g] {
			return fmt.Errorf("declared supplementary group %s is ABSENT (Groups: %s, want %v): "+
				"the declaration does not describe this pod, so the posture is not the one named",
				g, groupsField, groupSet(want))
		}
	}
	return nil
}

func requirePosture(wantUID int, declared []int) error {
	// runAsNonRoot: true is the one field of the chart's pod securityContext
	// that a plain container CAN honour, and it was the one this check did not
	// require: `--uid 0` reported every leg held, indistinguishable from a
	// real run.
	if wantUID == 0 {
		return fmt.Errorf("uid 0 is never the posture to measure: the chart sets runAsNonRoot: true")
	}
	if got := os.Getuid(); got != wantUID {
		return fmt.Errorf("uid %d, want %d", got, wantUID)
	}
	// EQUAL, not merely non-zero. The chart sets runAsGroup to the same value
	// as runAsUser, and the first version of this check only refused 0 while
	// its own message named the value — `--user 10001:65534` reported every
	// leg held.
	if got := os.Getgid(); got != wantUID {
		return fmt.Errorf("gid %d, want %d: the chart sets runAsGroup to runAsUser", got, wantUID)
	}
	status := readFile("/proc/self/status")
	if len(status) == 0 {
		return fmt.Errorf("/proc/self/status is unreadable, so the posture cannot be established")
	}
	field := func(name string) string {
		for _, line := range strings.Split(string(status), "\n") {
			if rest, ok := strings.CutPrefix(line, name+":"); ok {
				return strings.TrimSpace(rest)
			}
		}
		return ""
	}
	if got := field("Seccomp"); got != "2" {
		return fmt.Errorf("the Seccomp field is %q, want \"2\" (SECCOMP_MODE_FILTER): no filter is in force", got)
	}
	// Advisory, not required: the field only exists since Linux 5.9, and
	// `Seccomp: 2` already proves a filter is in force. A zero count with
	// that mode is a contradiction worth refusing; an ABSENT field is not.
	if got := field("Seccomp_filters"); got == "0" {
		return fmt.Errorf("the Seccomp_filters field is %q: the mode says filter, the count says none", got)
	}
	// And the filter's EFFECT, because the two fields above name the MODE and
	// never the PROFILE. A filter that denies nothing reports exactly the same
	// two values: measured, an allow-all profile gave `Seccomp: 2` with one
	// filter loaded and every leg held — attributing a verdict to a posture
	// nothing had read. That is the same defect this bench refuses for the
	// runner image and the kind node image, and the profile is the one input
	// it exists to judge.
	if err := requireDeniedSyscalls(); err != nil {
		return err
	}
	// And the SUPPLEMENTARY groups, which is where fsGroup lands. os.Getgid()
	// returns the PRIMARY gid and cannot see them: measured, `--group-add 0`
	// leaves `Gid: 10001 …` untouched and writes `Groups: 0 10001`, and a pod
	// with fsGroup 0 reported every leg held while its scratch emptyDir came
	// out root-group and group-writable. The chart sets fsGroup to runAsUser
	// (values.yaml:891), so any other group is a widening of the boundary the
	// pod IS.
	//
	if err := checkGroups(field("Groups"), wantUID, declared); err != nil {
		return err
	}
	if got := field("NoNewPrivs"); got != "1" {
		return fmt.Errorf("NoNewPrivs %q, want \"1\": privilege escalation is not blocked", got)
	}
	// BOTH the effective and the BOUNDING set must be empty. CapEff alone was
	// the hole: an unprivileged uid has CapEff 0 whatever it is allowed to
	// acquire, so a --privileged container with a permissive seccomp profile
	// passed this check and reported every leg held — with the host's disks
	// visible inside it. Measured: the chart's `capabilities: drop: [ALL]`
	// gives CapBnd 0000000000000000, that container gives 000001ffffffffff.
	//
	// A hex field of any width is compared by VALUE, not by its text, so a
	// kernel that widens the field does not silently satisfy this.
	for _, name := range []string{"CapEff", "CapBnd"} {
		raw := field(name)
		n, err := strconv.ParseUint(raw, 16, 64)
		if err != nil {
			return fmt.Errorf("%s %q is not a hex mask this build can read: %w", name, raw, err)
		}
		if n != 0 {
			return fmt.Errorf("%s %s: %d capability bits are still there, so this container does not "+
				"carry the chart's capabilities: drop: [ALL]", name, raw, bits.OnesCount64(n))
		}
	}
	return nil
}

// deniedBySeccompByDefault is the SET a container runtime's default profile
// refuses to an unprivileged process, and probing one member of it was not
// enough. Measured on the shipped image, same posture flags, uid 10001:
//
//	                  daemon default   allow-all except add_key
//	add_key           EPERM            EPERM   <- the only one a single probe saw
//	keyctl            EPERM            ENOTSUP
//	bpf               EPERM            EINVAL
//	perf_event_open   EPERM            EACCES
//	unshare           EPERM            SUCCEEDED
//
// So a profile that bites exactly once satisfied a one-member probe and
// reported every leg held, while bpf, perf_event_open and unshare reached the
// kernel.
//
// THE SET IS A NECESSARY CONDITION, NOT A CHARACTERISATION, and the earlier
// wording ("as far as a process inside the container can tell") was wrong: a
// process inside the container can tell more, which is how a profile denying
// exactly the probed members was caught letting io_uring_setup through. What
// this establishes is that the filter refuses every member a runtime default
// refuses; the profile's identity is a separate question, asked of the runtime
// by each arm.
//
// Every argument here is deliberately invalid, so a probe that is ALLOWED
// through fails on its own arguments instead of doing anything.
// A member earns its place by DISCRIMINATING, not by reputation. `mount`,
// `init_module`, `kexec_load`, `pivot_root` are EPERM from the KERNEL for an
// unprivileged process whatever the filter says, so probing them witnesses
// nothing. io_uring_setup and mbind are the opposite and were missing:
// measured, they SUCCEED under a permissive filter and are EPERM under BOTH
// runtime defaults (docker 29.9.3 and containerd 2.1.1). Without them, a
// profile denying exactly the first five and allowing everything else was
// accepted while io_uring_setup returned a working fd.
var deniedBySeccompByDefault = []struct {
	name string
	nr   uintptr
	args [3]uintptr
}{
	{"keyctl", unix.SYS_KEYCTL, [3]uintptr{^uintptr(0), 0, 0}},
	{"bpf", unix.SYS_BPF, [3]uintptr{^uintptr(0), 0, 0}},
	{"perf_event_open", unix.SYS_PERF_EVENT_OPEN, [3]uintptr{0, 0, 0}},
	{"unshare", unix.SYS_UNSHARE, [3]uintptr{0, 0, 0}},
	{"io_uring_setup", unix.SYS_IO_URING_SETUP, [3]uintptr{0, 0, 0}},
	{"mbind", unix.SYS_MBIND, [3]uintptr{0, 0, 0}},
}

// requireDeniedSyscalls asks the KERNEL what this container's seccomp filter
// actually refuses, over the set above plus add_key, and reports the whole
// fingerprint rather than a boolean.
//
// add_key is kept first because it is the one with a forgery-proof corroborant:
// a filter can answer ENOSYS for it and claim "this kernel has no keyring",
// and procfs says otherwise. That hatch was controlled by the very input under
// judgement — measured, a profile returning ENOSYS for add_key produced "NOT
// COVERED" and a green while /proc/sys/kernel/keys/maxkeys read 200.
func requireDeniedSyscalls() error {
	var allowed []string

	_, err := unix.AddKey("user", "inpodbench-probe", []byte("x"), -8 /* KEY_SPEC_THREAD_KEYRING */)
	switch {
	case errors.Is(err, unix.EPERM):
	case errors.Is(err, unix.ENOSYS):
		switch exists, readable := keyringExists(); {
		case !readable:
			return fmt.Errorf("add_key answered ENOSYS and /proc/sys/kernel/keys/maxkeys is unreadable, " +
				"so nothing corroborates it: the filter under judgement must not be the only witness")
		case exists:
			allowed = append(allowed, "add_key=ENOSYS while this kernel HAS a keyring")
		default:
			fmt.Println("NOTE add_key is ENOSYS and this kernel has no keyring, so that probe is NOT covered here")
		}
	default:
		allowed = append(allowed, fmt.Sprintf("add_key=%v", errText(err)))
	}

	for _, p := range deniedBySeccompByDefault {
		_, _, errno := unix.Syscall(p.nr, p.args[0], p.args[1], p.args[2])
		if errno != unix.EPERM {
			allowed = append(allowed, fmt.Sprintf("%s=%v", p.name, errText(errno)))
		}
	}

	if len(allowed) > 0 {
		return fmt.Errorf("this container's seccomp filter let %d of %d probes reach the kernel (%s): "+
			"a runtime default refuses all of them to an unprivileged process, so `Seccomp: 2` above "+
			"names a mode and this filter is not that default",
			len(allowed), len(deniedBySeccompByDefault)+1, strings.Join(allowed, ", "))
	}
	return nil
}

// keyringExists corroborates an ENOSYS from add_key with something the filter
// under judgement cannot forge. Measured: runc refuses to mount anything over
// a path inside /proc, on both the docker and the Kubernetes subPath route, so
// the corroborant cannot be masked through the run line.
//
// An UNREADABLE corroborant is not an absent keyring: it returns the third
// state so the caller refuses rather than accepting the hatch on silence.
func keyringExists() (exists, readable bool) {
	raw := strings.TrimSpace(string(readFile("/proc/sys/kernel/keys/maxkeys")))
	if raw == "" {
		return false, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return false, false
	}
	return n > 0, true
}

func errText(err error) string {
	if err == nil {
		return "SUCCEEDED"
	}
	if errno, ok := err.(unix.Errno); ok && errno == 0 {
		return "SUCCEEDED"
	}
	return err.Error()
}

// describeSelf is the provenance line the evidence was missing: which binary,
// which kernel. Without it a log cannot be attributed to anything.
func describeSelf(binary string) string {
	kernel := strings.TrimSpace(string(readFile("/proc/sys/kernel/osrelease")))
	sum := "unreadable"
	if b, err := os.ReadFile(binary); err == nil {
		sum = fmt.Sprintf("sha256:%x", sha256.Sum256(b))
	}
	return fmt.Sprintf("child=%s %s kernel=%s uid=%d", binary, sum, kernel, os.Getuid())
}
