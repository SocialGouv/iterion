package execenv

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// TryMain is called before application initialization, from the actual main.
// It cannot precede Go runtime/package initialization. The caller MUST exit
// when handled: this dedicated process owns an unawaited stdin copy goroutine.
func TryMain(argv []string, stdin io.Reader, stdout, stderr io.Writer) (handled bool, code int) {
	if len(argv) < 2 || !strings.HasPrefix(argv[1], "__exec-env-") {
		return false, 0
	}
	fail := func(err error) (bool, int) { fmt.Fprintln(stderr, err); return true, 125 }
	if len(argv) != 2 || argv[1] != HostMode {
		return fail(refusal("host", "mode", nil))
	}
	if err := hostSupported(); err != nil {
		return fail(err)
	}
	l, clean, err := readWireEnvironment(os.Environ())
	if err != nil {
		return fail(err)
	}
	code, err = runHost(l, clean, stdin, stdout, stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
	}
	return true, code
}
