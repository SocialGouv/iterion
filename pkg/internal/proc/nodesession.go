package proc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// What an agent node starts ends with the node
// (docs/adr/127-an-agent-nodes-processes-end-with-the-node.md). The engine
// draws one token per delegation and exports it to the agent's process as a
// variable of its own, NodeSessionVar(token)=1: every descendant inherits it,
// and the variables of nested delegations accumulate instead of replacing one
// another. When the delegation ends, a sweep stops the processes that carry the
// entry, every descendant of them — a process that rewrote its title over its
// environment is still a descendant — and every process an earlier scan of the
// same sweep found.

// NodeSessionPrefix begins the name of every node session variable.
const NodeSessionPrefix = "ITERION_NODE_SESSION_"

// NodeSessionGrace bounds how long a sweep waits for the processes it sent
// TERM to exit before it kills what is left.
const NodeSessionGrace = 5 * time.Second

// NodeSessionKillWait bounds how long a sweep waits for the processes it sent
// KILL to disappear; one still there after it is a survivor.
const NodeSessionKillWait = 3 * time.Second

// sweepGrace and sweepKillWait are the bounds a sweep runs with: the
// constants above, which a test may shorten.
var sweepGrace, sweepKillWait = NodeSessionGrace, NodeSessionKillWait

// sweepGateWaitSeconds bounds, in seconds, how long a sweep program's scan
// gate waits for its release before the program fails with an error. It is
// shorter than a witness's own ceiling, so a held sweep never outlives its
// test; a test that wants a shorter one sets it.
var sweepGateWaitSeconds = 20

// sweepScanGateFile, empty in production, is the path a sweep program reports
// its scans to and waits at: after every scan it appends what the scan found
// of the session to "<path>.scanned" and waits for "<path>.resume" to exist,
// before it acts on that scan. It is the seam a test holds a sweep program at,
// to stage a process that shows between two scans instead of guessing when the
// window between them opens; sweepScanGate is its counterpart for the sweep
// that runs here.
var sweepScanGateFile string

// NodeSessionSettle is the interval between a sweep's scans. A sweep ends on
// two scans in a row that find nothing of the session alive: a process in the
// middle of an exec has no readable environment for an instant, and one scan
// would miss it.
const NodeSessionSettle = 50 * time.Millisecond

// ErrNodeSessionSweepUnsupported reports a platform with no /proc: the
// processes a node leaves behind cannot be found there.
var ErrNodeSessionSweepUnsupported = errors.New("no /proc on this platform: the processes a node leaves behind cannot be found")

// NewNodeSessionToken draws the token for one delegation: lowercase base32,
// about 130 random bits.
func NewNodeSessionToken() string { return strings.ToLower(rand.Text()) }

// NodeSessionVar is the name of the variable that carries token: the prefix
// and a hash of the token, in lowercase hex. A name a token spelled could hold
// KEY, TOKEN or SECRET, words on which an agent's own sandbox drops a variable
// from the commands it runs (codex's default shell environment policy); hex
// cannot spell them.
func NodeSessionVar(token string) string {
	sum := sha256.Sum256([]byte(token))
	return NodeSessionPrefix + hex.EncodeToString(sum[:16])
}

// NodeSessionEntry is the environment entry that carries token.
func NodeSessionEntry(token string) string { return NodeSessionVar(token) + "=1" }

type nodeSessionEnvKey struct{}

// WithNodeSessionEnv returns ctx carrying token's entry next to those it
// already carries, for the spawners that build a process's environment from
// their context rather than from a delegation's: a nested delegation's
// entries accumulate.
func WithNodeSessionEnv(ctx context.Context, token string) context.Context {
	prev := NodeSessionEnvFrom(ctx)
	return context.WithValue(ctx, nodeSessionEnvKey{}, append(prev[:len(prev):len(prev)], NodeSessionEntry(token)))
}

// NodeSessionEnvFrom returns the node session entries ctx carries, for a
// spawner to add to the environment of what it starts.
func NodeSessionEnvFrom(ctx context.Context) []string {
	env, _ := ctx.Value(nodeSessionEnvKey{}).([]string)
	return env
}

// A token is matched as a whole environment entry and embedded in a shell
// program, so it is restricted to characters neither can misread.
var nodeSessionTokenRe = regexp.MustCompile(`^[a-z0-9-]{8,}$`)

func checkNodeSessionToken(token string) error {
	if !nodeSessionTokenRe.MatchString(token) {
		return fmt.Errorf("node session token %q: want at least 8 of [a-z0-9-]", token)
	}
	return nil
}

// BootTicks is a point in time the way the kernel dates a process's start:
// clock ticks (USER_HZ, 100 on Linux) since boot.
type BootTicks uint64

// userHZ is the unit of the start times /proc reports; the kernel fixes it at
// 100 for user space on every architecture it supports.
const userHZ = 100

// SessionProcess is one process of a node session.
type SessionProcess struct {
	PID  int
	Name string
}

// SweepReport is what a sweep did.
type SweepReport struct {
	// Stopped were signalled and are gone.
	Stopped []SessionProcess
	// Survivors were signalled and still ran when the sweep gave up.
	Survivors []SessionProcess
	// Unreadable ran under this uid, started after the session began, and
	// their environment could not be read: they may carry the token.
	Unreadable []SessionProcess
}

// Labels names the stopped processes the way a delegation reports the
// background work it lost.
func (r SweepReport) Labels() []string {
	out := make([]string, 0, len(r.Stopped))
	for _, p := range r.Stopped {
		out = append(out, fmt.Sprintf("process %s (pid %d), stopped at the end of the node", p.Name, p.PID))
	}
	return out
}

// Problems is what a sweep could not do, or nil: the survivors, and the
// processes it could not look at.
func (r SweepReport) Problems() error {
	var parts []string
	if len(r.Survivors) > 0 {
		parts = append(parts, "still running after KILL: "+describeProcesses(r.Survivors))
	}
	if len(r.Unreadable) > 0 {
		parts = append(parts, "environment unreadable, may carry the token: "+describeProcesses(r.Unreadable))
	}
	if len(parts) == 0 {
		return nil
	}
	return errors.New(strings.Join(parts, "; "))
}

func describeProcesses(ps []SessionProcess) string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, fmt.Sprintf("%s (pid %d)", p.Name, p.PID))
	}
	return strings.Join(out, ", ")
}

// The line heads NodeSessionSweepScript prints and ParseNodeSessionSweep reads.
const (
	sweepLineHead       = "iterion-node-session "
	sweepLineStopped    = sweepLineHead + "stopped "
	sweepLineSurvivor   = sweepLineHead + "survivor "
	sweepLineUnreadable = sweepLineHead + "unreadable "
	sweepLineError      = sweepLineHead + "error "
	sweepLineDone       = sweepLineHead + "done"
)

// NodeSessionSweepScript returns the POSIX sh program that sweeps token inside
// the container it runs in, the way SweepNodeSession sweeps it here. sinceFile
// names a file holding the BootTicks the session began at — the spawn wrapper
// writes it — and may be empty: every unreadable process of this uid is then
// reported. The program prints one line per process it stopped, per survivor
// and per unreadable process, and an error line when it cannot look; its last
// line says it finished, so a program that died on the way is told from one
// that found nothing. It exits 0.
func NodeSessionSweepScript(token, sinceFile string) (string, error) {
	if err := checkNodeSessionToken(token); err != nil {
		return "", err
	}
	if strings.ContainsAny(sinceFile, "'\n") {
		return "", fmt.Errorf("node session since file %q: a quote or a newline cannot be embedded", sinceFile)
	}
	gateFile := sweepScanGateFile
	if strings.ContainsAny(gateFile, "'\n\\") {
		return "", fmt.Errorf("node session scan gate %q: a quote, a backslash or a newline cannot be embedded", gateFile)
	}
	return `E='` + NodeSessionEntry(token) + `'
H='` + sweepLineHead + `'
SINCE_FILE='` + sinceFile + `'
GATE='` + gateFile + `'
GATE_WAIT=` + strconv.Itoa(sweepGateWaitSeconds) + `
GRACE=` + strconv.Itoa(int(sweepGrace/time.Second)) + `
KILLWAIT=` + strconv.Itoa(int(sweepKillWait/time.Second)) + `
SETTLE=` + strconv.FormatFloat(NodeSessionSettle.Seconds(), 'f', -1, 64) + `
` + nodeSessionSweepBody, nil
}

// nodeSessionSweepBody is the program NodeSessionSweepScript completes. It
// keeps every fact it learns about a process in variables named after its pid
// (pp_<pid> its parent, st_<pid> its start, nm_<pid> its name), assigned from
// /proc through eval with the value escaped, never interpolated: a name is
// chosen by the process it belongs to.
const nodeSessionSweepBody = `for c in tr grep date sleep; do
  if ! command -v "$c" >/dev/null 2>&1; then echo "${H}error $c is missing in this container"; exit 0; fi
done
if ! { read -r _ < /proc/self/stat; } 2>/dev/null; then echo "${H}error no readable /proc in this container"; exit 0; fi
SINCE=0
if [ -n "$SINCE_FILE" ] && [ -f "$SINCE_FILE" ] && { read -r s < "$SINCE_FILE"; } 2>/dev/null; then
  case $s in ''|*[!0-9]*) ;; *) SINCE=$s;; esac
fi
self=$$
me=
{ while read -r k v _; do if [ "$k" = Uid: ]; then me=$v; break; fi; done < /proc/self/status; } 2>/dev/null
# One grep over every environment per scan when this grep reads NUL-separated
# records; otherwise one tr and one grep per process. Both read whole entries:
# tr turns NULs into newlines only after turning newlines into \001, so a
# value holding a newline cannot forge a line.
if printf 'a\000' | grep -qzx a 2>/dev/null; then fastz=1; else fastz=; fi
settle() { sleep "$SETTLE" 2>/dev/null || sleep 1; }
# gate MEMBERS: with GATE set — a test holding this sweep at the scan it has
# just finished, never in production — report what the scan found and wait for
# the test to let the sweep act on it. A gate nobody releases is an error, so
# that a held sweep is never read as a sweep that found nothing.
gate() {
  [ -n "$GATE" ] || return 0
  printf '%s\n' "$1" >> "$GATE.scanned" 2>/dev/null || {
    printf '%serror the scan gate at %s is not writable\n' "$H" "$GATE"
    exit 0
  }
  gate_deadline=$(( $(date +%s) + GATE_WAIT ))
  while [ ! -f "$GATE.resume" ]; do
    if [ "$(date +%s)" -gt "$gate_deadline" ]; then
      printf '%serror the scan gate at %s was never released\n' "$H" "$GATE"
      exit 0
    fi
    settle
  done
  printf '' > "$GATE.passed"
}
# slurp FILE: sets line to the whole of FILE, its lines joined by spaces — a
# process name may hold a newline, and the fields that follow it are on the
# last line. Status 1 when FILE cannot be read.
slurp() {
  line=
  { true < "$1"; } 2>/dev/null || return 1
  { while IFS= read -r l || [ -n "$l" ]; do line="$line$l "; done < "$1"; } 2>/dev/null
  [ -n "$line" ]
}
# stat_of PID: sets ok, state, ppid, start and name from /proc/PID/stat. The
# name is between the first ( and the last ), whatever it holds; every field
# after the last ") " is a number or a state letter.
stat_of() {
  ok=
  slurp "/proc/$1/stat" || return 0
  name=${line#*(}; name=${name%)*}
  set -- ${line##*) }
  state=$1; ppid=$2; start=${20}
  case $ppid in ''|*[!0-9]*) return 0;; esac
  case $start in ''|*[!0-9]*) return 0;; esac
  ok=1
}
# alive PID: the process runs — a zombie leader runs while one of its threads
# does.
alive() {
  stat_of "$1"; [ -n "$ok" ] || return 1
  [ "$state" != Z ] && return 0
  for t in /proc/$1/task/[0-9]*; do
    slurp "$t/stat" || continue
    set -- ${line##*) }
    [ "$1" != Z ] && return 0
  done
  return 1
}
# holds FILE: the environment FILE holds the entry; status 2 when FILE cannot
# be read. Opened by true, never by ':': a redirection that fails on a special
# built-in ends a POSIX shell.
holds() {
  { true < "$1"; } 2>/dev/null || return 2
  if [ -n "$fastz" ]; then grep -qsFxz -- "$E" "$1" 2>/dev/null; return; fi
  tr '\000\n' '\n\001' 2>/dev/null < "$1" | grep -qFx -- "$E"
}
# carrier PID: the process's environment holds the entry, read through its
# threads when the process-wide one cannot be; status 2 when none can.
carrier() {
  holds "/proc/$1/environ"; r=$?
  [ $r -ne 2 ] && return $r
  for t in /proc/$1/task/[0-9]*; do
    holds "$t/environ"; r=$?
    [ $r -ne 2 ] && return $r
  done
  return 2
}
uid_of() {
  u=
  { while read -r k v _; do if [ "$k" = Uid: ]; then u=$v; break; fi; done < "/proc/$1/status"; } 2>/dev/null
}
known=' '
termed=' '
unread=' '
# scan: sets members to the live processes of the session — carriers, known
# processes still the same, and every descendant of either.
scan() {
  all=' '
  base=' '
  cand=' '
  if [ -n "$fastz" ]; then
    for f in $(grep -lsFxz -- "$E" /proc/[0-9]*/environ 2>/dev/null); do
      p=${f#/proc/}; cand="$cand${p%/environ} "
    done
  fi
  for d in /proc/[0-9]*; do
    p=${d#/proc/}
    if [ "$p" = 1 ] || [ "$p" = "$self" ]; then continue; fi
    alive "$p" || continue
    stat_of "$p"; [ -n "$ok" ] || continue
    eval "pp_$p=\$ppid; nm_$p=\$name; cur_$p=\$start"
    all="$all$p "
    case $known in *" $p:$start "*) base="$base$p "; continue;; esac
    if [ -n "$fastz" ]; then
      case $cand in *" $p "*) eval "st_$p=\$start"; base="$base$p "; continue;; esac
      { true < "$d/environ"; } 2>/dev/null && continue
    fi
    carrier "$p"; r=$?
    if [ $r -eq 0 ]; then eval "st_$p=\$start"; base="$base$p "; continue; fi
    if [ $r -eq 2 ] && [ "$start" -ge "$SINCE" ]; then
      uid_of "$p"
      if [ -n "$me" ] && [ "$u" = "$me" ]; then
        case $unread in *" $p:$start "*) ;; *) unread="$unread$p:$start ";; esac
      fi
    fi
  done
  members=' '
  for p in $all; do
    q=$p; n=0
    while [ "$q" -gt 1 ] && [ $n -lt 512 ]; do
      case $base in *" $q "*) members="$members$p "; break;; esac
      eval "q=\${pp_$q:-0}"; n=$((n+1))
    done
  done
  for p in $members; do
    case $known in *" $p:"*) ;; *) eval "known=\"\${known}$p:\$cur_$p \"";; esac
  done
}
# same PID START: PID is still the process the sweep found.
same() { stat_of "$1"; [ -n "$ok" ] && [ "$start" = "$2" ]; }
# phase SIGNAL DEADLINE: signal what each scan finds — TERM once per process,
# KILL every round — until two scans in a row find nothing (status 0), or
# until DEADLINE once a round has signalled (status 1).
phase() {
  quiet=0
  while :; do
    scan
    gate "$members"
    if [ "$members" = ' ' ]; then
      quiet=$((quiet+1))
      [ $quiet -ge 2 ] && return 0
    else
      quiet=0
      for p in $members; do
        eval "s=\$cur_$p"
        same "$p" "$s" || continue
        if [ "$1" = TERM ]; then
          case $termed in *" $p "*) continue;; esac
          termed="$termed$p "
        fi
        kill -"$1" "$p" 2>/dev/null
      done
    fi
    [ "$(date +%s)" -gt "$2" ] && return 1
    settle
  done
}
# A TERM phase that ends quiet leaves nothing for KILL to find.
phase TERM $(( $(date +%s) + GRACE )) || phase KILL $(( $(date +%s) + KILLWAIT ))
# A KILL is delivered at once but a process takes a moment to exit: give the
# last ones that moment before calling them survivors.
i=0
while [ $i -lt 10 ]; do
  left=
  for e in $known; do same "${e%%:*}" "${e#*:}" && alive "${e%%:*}" && left=1 && break; done
  [ -z "$left" ] && break
  settle; i=$((i+1))
done
for e in $known; do
  p=${e%%:*}; s=${e#*:}
  eval "n=\${nm_$p:-?}"
  if same "$p" "$s" && alive "$p"; then
    printf '%ssurvivor %s %s\n' "$H" "$p" "$n"
  else
    printf '%sstopped %s %s\n' "$H" "$p" "$n"
  fi
done
for e in $unread; do
  p=${e%%:*}; s=${e#*:}
  case $known in *" $p:"*) continue;; esac
  if same "$p" "$s" && alive "$p"; then
    eval "n=\${nm_$p:-?}"
    printf '%sunreadable %s %s\n' "$H" "$p" "$n"
  fi
done
echo "${H}done"
exit 0
`

// ParseNodeSessionSweep reads NodeSessionSweepScript's output: what it did, and
// an error when it could not look or did not finish.
func ParseNodeSessionSweep(out string) (SweepReport, error) {
	var r SweepReport
	var errs []string
	done := false
	for _, line := range strings.Split(out, "\n") {
		var dst *[]SessionProcess
		var rest string
		switch {
		case line == sweepLineDone:
			done = true
			continue
		case strings.HasPrefix(line, sweepLineStopped):
			dst, rest = &r.Stopped, strings.TrimPrefix(line, sweepLineStopped)
		case strings.HasPrefix(line, sweepLineSurvivor):
			dst, rest = &r.Survivors, strings.TrimPrefix(line, sweepLineSurvivor)
		case strings.HasPrefix(line, sweepLineUnreadable):
			dst, rest = &r.Unreadable, strings.TrimPrefix(line, sweepLineUnreadable)
		case strings.HasPrefix(line, sweepLineError):
			errs = append(errs, strings.TrimPrefix(line, sweepLineError))
			continue
		default:
			continue
		}
		fields := strings.SplitN(rest, " ", 2)
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			errs = append(errs, fmt.Sprintf("unreadable sweep line %q", line))
			continue
		}
		name := ""
		if len(fields) == 2 {
			name = strings.TrimSpace(fields[1])
		}
		if name == "" {
			name = "?"
		}
		*dst = append(*dst, SessionProcess{PID: pid, Name: name})
	}
	if !done && len(errs) == 0 {
		errs = append(errs, "the sweep ended before its last line: what it stopped and what it left are not all known")
	}
	if len(errs) > 0 {
		return r, errors.New(strings.Join(errs, "; "))
	}
	return r, nil
}
