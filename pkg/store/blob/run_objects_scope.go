package blob

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/store"
)

// IncarnationRunObjects binds every operation, including cleanup and signed
// object URLs, to one original physical scope. It does not enable run birth.
// Artifacts and IR remain separate capabilities, as with ValueFormatRunObjects.
type IncarnationRunObjects interface {
	RunObjectsForScope(store.IncarnationDataScope) (RunObjects, error)
}

// SelectScopedRunObjects requires incarnation capability and never uses a legacy view.
func SelectScopedRunObjects(client Client, scope store.IncarnationDataScope) (RunObjects, error) {
	if _, err := scope.BlobPrefix(); err != nil {
		return nil, err
	}
	capable, ok := client.(IncarnationRunObjects)
	if !ok {
		return nil, fmt.Errorf("%w: incarnation blob RunObjects", store.ErrRunDataCapability)
	}
	return capable.RunObjectsForScope(scope)
}

func (c *S3Client) RunObjectsForScope(scope store.IncarnationDataScope) (RunObjects, error) {
	if _, err := scope.BlobPrefix(); err != nil {
		return nil, err
	}
	copy := *c
	copy.runObjectsFormat = scope.Header().Format
	copy.runObjectsScope = &scope
	return &s3RunObjects{client: copy}, nil
}

func (c *S3Client) runObjectKey(runID, key string) (string, error) {
	if c.runObjectsScope == nil {
		return runObjectKeyForFormat(c.runObjectsFormat, key), nil
	}
	return scopedRunDataKey(*c.runObjectsScope, runID, key)
}

// key has already passed the domain's existing path validation. Keep the
// trailing slash on domain roots so a sweep cannot match a sibling prefix.
func scopedRunDataKey(scope store.IncarnationDataScope, runID, key string) (string, error) {
	prefix, err := scope.BlobPrefix()
	if err != nil {
		return "", err
	}
	if runID != scope.Header().RunID {
		return "", store.ErrRunDataIdentityChanged
	}
	domain, rest, ok := strings.Cut(key, "/")
	switch domain {
	case "attachments", "tools", "sessions", "runfiles", "artifacts":
	default:
		return "", store.ErrRunDataCapability
	}
	if !ok || !strings.HasPrefix(rest, runID+"/") {
		return "", store.ErrInvalidRunDataIdentity
	}
	return prefix + domain + "/" + strings.TrimPrefix(rest, runID+"/"), nil
}

// AttachmentKeyForScope is the StorageRef for this exact incarnation only.
func AttachmentKeyForScope(scope store.IncarnationDataScope, runID, name, filename string) (string, error) {
	key, err := attachmentKey(runID, name, filename)
	if err != nil {
		return "", err
	}
	return scopedRunDataKey(scope, runID, key)
}

var _ IncarnationRunObjects = (*S3Client)(nil)
