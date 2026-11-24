package mongo

import "github.com/SocialGouv/iterion/pkg/store/blob"

// Separate backing maps model the closed namespaces; the production S3
// key layout is exercised independently through real HTTP in blob's tests.
func (b *valueFormatTestBlob) RunObjectsForFormat(f int) (blob.RunObjects, error) {
	x, e := b.space(f)
	if e != nil {
		return nil, e
	}
	return blob.SelectRunObjects(x, 1)
}
