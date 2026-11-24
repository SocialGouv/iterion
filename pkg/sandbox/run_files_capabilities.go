package sandbox

import (
	"context"
	"os"
)

// RootOpener returns an independently owned root; the caller closes it.
type RootOpener interface {
	OpenRoot(context.Context) (*os.Root, error)
}

// BindPathIdentity checks a captured host directory, not a replacement path.
type BindPathIdentity interface {
	HostPath() string
	ValidateHostPath(context.Context) error
}
