package blob

import "context"

// ValueFormatArtifacts keeps artifact namespaces disjoint by value format.
// Format1 retains the historical canonical namespace. A format2 namespace must
// be outside every legacy DeleteRun prefix. IDs remain unchanged. Unknown
// formats fail before I/O. This capability alone does not activate a writer.
type ValueFormatArtifacts interface {
	PutArtifactForFormat(context.Context, int, string, string, int, []byte) error
	GetArtifactForFormat(context.Context, int, string, string, int) ([]byte, error)
	ListArtifactVersionsForFormat(context.Context, int, string, string) ([]int, error)
	DeleteRunArtifactsForFormat(context.Context, int, string) error
}
