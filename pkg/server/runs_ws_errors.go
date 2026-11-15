package server

import "github.com/SocialGouv/iterion/pkg/runview/runstream"

func retainRunStreamFatal(first, next error) error {
	if first == nil && runstream.IsPermanentError(next) {
		return next
	}
	return first
}

// drainRunStreamFatal inspects only errors already queued when data closes.
// The producer contract publishes fatal errors before closing data. A producer
// that leaves Errors open or keeps adding warnings cannot hold completion open.
func drainRunStreamFatal(first error, errs <-chan error) error {
	pending := len(errs)
	for i := 0; i < pending; i++ {
		select {
		case err, ok := <-errs:
			if !ok {
				return first
			}
			first = retainRunStreamFatal(first, err)
		default:
			return first
		}
	}
	return first
}
