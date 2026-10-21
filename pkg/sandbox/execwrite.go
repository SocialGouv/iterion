package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
)

// writeFileScript is the in-sandbox writer WriteFileExec runs via
// `sh -c`. The destination path arrives as the positional parameter $1
// (no shell interpolation of the path) and the CONTENT arrives on stdin
// (never argv/env — both are visible to `ps` and, on kubernetes, to
// anyone who can read the exec request). umask 077 makes both the
// created parent dirs (0700) and the temp file (0600) owner-only; the
// final mv is atomic so a concurrent reader never sees a torn write, and
// each writer has a temp file of its own, so two writers of one path never
// mix, and a signal that ends the writer takes its temp file with it. A
// destination that is a directory is refused: mv would put the file inside
// it. $2 is the content's length: a stdin stream cut short leaves no file at
// all, never a prefix of the content in place of the whole. The writer's last
// line proves the write: the exec client's status alone proves nothing.
const writeFileScript = `set -e
umask 077
d=$(dirname "$1")
mkdir -p "$d"
t="$1.tmp.$$"
trap 'rm -f "$t"' EXIT
trap 'rm -f "$t"; exit 1' HUP INT TERM
cat > "$t"
n=$(( $(wc -c < "$t") ))
[ "$n" -eq "$2" ] || { echo "received $n of $2 bytes" >&2; exit 1; }
[ ! -d "$1" ] || { echo "$1 is a directory" >&2; exit 1; }
mv "$t" "$1"
echo "` + writeFileDone + ` $n"`

// writeFileDone heads the line the writer prints once the file is in place.
const writeFileDone = "iterion-written"

// WriteFileExec writes value to absPath inside the sandbox through the
// Run's exec seam. It is the shared write-through primitive behind
// mid-run credential propagation into copy-based sandboxes (kubernetes):
// the runtime uses it to seed the writable Claude config dir, the
// kubernetes driver to implement [WorkspaceFileRefresher].
//
// The value is streamed over stdin and never logged; errors embed only
// the path and the subprocess' stderr.
func WriteFileExec(ctx context.Context, r Run, absPath string, value []byte) error {
	if r == nil {
		return fmt.Errorf("sandbox: write file %s: nil run", absPath)
	}
	if !path.IsAbs(absPath) || path.Clean(absPath) != absPath || absPath == "/" {
		return fmt.Errorf("sandbox: write file: path %q must be a clean absolute file path", absPath)
	}
	if strings.ContainsAny(absPath, "\n\r\x00") {
		return fmt.Errorf("sandbox: write file: path %q contains a control character", absPath)
	}
	res, err := r.Exec(ctx, []string{"sh", "-c", writeFileScript, "sh", absPath, strconv.Itoa(len(value))}, ExecOpts{
		Stdin: bytes.NewReader(value),
	})
	if err != nil {
		return fmt.Errorf("sandbox: write file %s: %w", absPath, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("sandbox: write file %s: writer exited %d: %s",
			absPath, res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	if !slices.Contains(strings.Split(string(res.Stdout), "\n"), writeFileDone+" "+strconv.Itoa(len(value))) {
		return fmt.Errorf("sandbox: write file %s: %w: the writer never said the file was in place", absPath, ErrExecStatusUnknown)
	}
	return nil
}
