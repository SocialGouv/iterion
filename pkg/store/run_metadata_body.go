package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
)

// bindRunMetadataMutation captures only the original format, never execution
// authority. The caller revalidates this binding under the stable lock at
// publication, after streaming or other work outside that lock.
func (s *FilesystemRunStore) bindRunMetadataMutation(ctx context.Context, id string) (context.Context, error) {
	parent, unlock, err := s.lockRunMetadataMutation(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if parent == nil {
		parent = &Run{ID: id, FormatVersion: 1}
	}
	original, err := CaptureRunValueFormat(parent)
	if err != nil {
		return nil, err
	}
	return WithRunValueFormat(ctx, original)
}

// stagedRunBody retains strict source bytes from creation through publication.
// The legacy branch keeps its historical closed-temporary rename behavior.
type stagedRunBody struct {
	path, name string
	file       *os.File
	root       *os.Root
	size       int64
}

func (b *stagedRunBody) Close() error {
	if b.root == nil {
		return os.Remove(b.path)
	}
	return errors.Join(b.file.Close(), b.root.Remove(b.name), b.root.Close())
}
func (b *stagedRunBody) publish(ctx context.Context, dst *runPayloadFile, perm os.FileMode) error {
	if dst.strict {
		if b.file == nil {
			return ErrRunDataIdentityChanged
		}
		return dst.writeReaderAtomic(ctx, io.NewSectionReader(b.file, 0, b.size), perm)
	}
	if b.file != nil {
		return ErrRunDataIdentityChanged
	}
	if err := os.Chmod(b.path, perm); err != nil {
		return err
	}
	return os.Rename(b.path, dst.key)
}

// stageRunMetadataBody drains arbitrary readers outside metadata locks. The
// caller owns Close and revalidates bound before creating the destination.
func (s *FilesystemRunStore) stageRunMetadataBody(ctx context.Context, id string, body io.Reader, sink io.Writer) (bound context.Context, staged *stagedRunBody, n int64, err error) {
	bound, err = s.bindRunMetadataMutation(ctx, id)
	if err != nil {
		return nil, nil, 0, err
	}
	if body == nil {
		return nil, nil, 0, fmt.Errorf("store: nil run body")
	}
	staged = &stagedRunBody{}
	var tmp *os.File
	if binding, ok := RunDataBindingFrom(bound); ok && binding.Header().Protocol == IncarnationRunData {
		staged.root, err = openRunDataStoreRoot(s.root)
		if err != nil {
			return nil, nil, 0, err
		}
		for {
			if err = bound.Err(); err != nil {
				staged.root.Close()
				return nil, nil, 0, err
			}
			staged.name = ".run-body-stage-" + rand.Text()
			tmp, err = staged.root.OpenFile(staged.name, os.O_RDWR|os.O_CREATE|os.O_EXCL, filePerm)
			if errors.Is(err, os.ErrExist) {
				continue
			}
			if err != nil {
				staged.root.Close()
				return nil, nil, 0, err
			}
			break
		}
		staged.file = tmp
	} else {
		tmp, err = os.CreateTemp(s.root, ".run-body-stage-*")
		if err != nil {
			return nil, nil, 0, err
		}
		staged.path = tmp.Name()
	}
	owned := staged
	defer func() {
		if err != nil {
			if owned.root == nil {
				_ = tmp.Close()
			}
			_ = owned.Close()
			staged = nil
		}
	}()
	var dst io.Writer = tmp
	if sink != nil {
		dst = io.MultiWriter(tmp, sink)
	}
	n, err = io.Copy(dst, runMetadataBodyReader{ctx: bound, source: body})
	if err != nil {
		return nil, staged, n, err
	}
	if err = tmp.Sync(); err != nil {
		return nil, staged, n, err
	}
	if owned.root == nil {
		if err = tmp.Close(); err != nil {
			return nil, staged, n, err
		}
	}
	owned.size = n
	return bound, owned, n, nil
}

type runMetadataBodyReader struct {
	ctx    context.Context
	source io.Reader
}

func (r runMetadataBodyReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.source.Read(p)
	if cancelled := r.ctx.Err(); cancelled != nil {
		return n, cancelled
	}
	return n, err
}
