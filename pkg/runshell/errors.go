package runshell

import "errors"

// ErrUnsupported marks a platform without PTY support (Windows). The
// server handler maps it to 501 so the desktop build stays green while
// the feature remains Unix-only.
var ErrUnsupported = errors.New("runshell: post-mortem shell is not supported on this platform")
