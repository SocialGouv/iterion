package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type streamRootLink struct {
	parent *os.Root
	name   string
	info   os.FileInfo
}
type filesystemStreamSelection struct {
	mu                  sync.Mutex
	s                   *FilesystemRunStore
	id, physical, watch string
	base, run, domain   *os.Root
	roots               []*os.Root
	links               []streamRootLink
	pending             *os.Root
	remaining           []string
	baseInfo            os.FileInfo
	present             bool
	header              RunDataHeader
	observation         RunObservation
	binding             RunDataBinding
	intent              RunValueFormat
	hasIntent           bool
	tenant, owner       string
	privileged          bool
	closed              bool
}

// streamChild represents initial absence separately from a failed identity
// check. A joined identity/ENOENT error never masquerades as an empty domain.
func streamChild(parent *os.Root, name string) (*os.Root, os.FileInfo, bool, error) {
	before, e := parent.Lstat(name)
	if errors.Is(e, os.ErrNotExist) {
		return nil, nil, false, nil
	}
	if e != nil {
		return nil, nil, false, e
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, nil, false, ErrRunDataIdentityChanged
	}
	child, e := parent.OpenRoot(name)
	if e != nil {
		return nil, nil, false, streamNodeError(e)
	}
	after, e := child.Stat(".")
	if e != nil {
		child.Close()
		return nil, nil, false, streamNodeError(e)
	}
	if !os.SameFile(before, after) {
		child.Close()
		return nil, nil, false, ErrRunDataIdentityChanged
	}
	return child, after, true, nil
}

// streamNodeError classifies a failed observation of a retained node. Only a
// failure saying the node is gone or is no longer what was retained (absent,
// not a directory, a link where none may be) refuses the stream as a
// replacement; any other failure (a permission, descriptor exhaustion, an I/O
// error) is returned as it is, transient for the caller.
func streamNodeError(e error) error {
	if errors.Is(e, os.ErrNotExist) || errors.Is(e, syscall.ENOTDIR) || errors.Is(e, syscall.ELOOP) {
		return errors.Join(ErrRunDataIdentityChanged, e)
	}
	return e
}
func (n *filesystemStreamSelection) addChild(parent *os.Root, name string) (*os.Root, bool, error) {
	root, info, found, e := streamChild(parent, name)
	if e != nil || !found {
		return nil, found, e
	}
	n.roots = append(n.roots, root)
	n.links = append(n.links, streamRootLink{parent, name, info})
	return root, true, nil
}
func streamReadFile(root *os.Root, name string) ([]byte, error) {
	f, _, e := openStreamRegular(root, name)
	if e != nil {
		return nil, e
	}
	raw, e := io.ReadAll(f)
	return raw, errors.Join(e, f.Close())
}
func streamHeader(ctx context.Context, root *os.Root, id string) (RunDataHeader, []byte, bool, error) {
	if _, e := root.Lstat(deletionMarkerName); e == nil {
		return RunDataHeader{}, nil, false, ErrRunDeleted
	} else if !errors.Is(e, os.ErrNotExist) {
		return RunDataHeader{}, nil, false, e
	}
	raw, e := streamReadFile(root, "run.json")
	if errors.Is(e, os.ErrNotExist) {
		return RunDataHeader{}, nil, false, nil
	}
	if e != nil {
		return RunDataHeader{}, nil, false, e
	}
	h, e := RunDataHeaderFromJSON(ctx, raw)
	if e != nil {
		if filesystemContextErrorOnly(e) {
			return h, nil, false, e
		}
		return h, nil, false, errors.Join(ErrInvalidRunDataIdentity, e, ctx.Err())
	}
	h, e = ValidateRunDataObservation(ctx, id, h)
	if e != nil {
		return h, nil, false, e
	}
	if e = RequireRunDataStorage(h.Protocol); e != nil {
		return h, nil, false, e
	}
	return h, raw, true, nil
}

func (s *FilesystemRunStore) OpenFilesystemStream(ctx context.Context, id string, opts FilesystemStreamOpenOptions) (result FilesystemStreamSelection, observed bool, err error) {
	if ctx == nil {
		return nil, false, ErrInvalidRunDataIdentity
	}
	if err = ctx.Err(); err != nil {
		return nil, false, err
	}
	if err = sanitizePathComponent("run ID", id); err != nil {
		// No retry can make an invalid ID valid.
		return nil, false, errors.Join(ErrInvalidRunDataIdentity, err)
	}
	if len(id) > 255 {
		// The one bound every platform shares: refuse before any syscall, so
		// a native long-name error cannot become an endless retry.
		return nil, false, errors.Join(ErrInvalidRunDataIdentity, fmt.Errorf("run id is %d bytes", len(id)))
	}
	if _, err = ValueFormatForWrite(ctx, id); err != nil {
		return nil, false, err
	}
	n := &filesystemStreamSelection{s: s, id: id, privileged: IsWithoutTenantFilter(ctx)}
	n.tenant, _ = TenantFromContext(ctx)
	n.owner, _ = OwnerFromContext(ctx)
	n.intent, n.hasIntent = ctx.Value(runValueFormatKey{}).(RunValueFormat)
	defer func() {
		if err == nil {
			err = ctx.Err()
		}
		if errors.Is(err, syscall.ENAMETOOLONG) {
			// The run ID is the only name this open varies: one too long for
			// the file system is as invalid as a malformed one.
			err = errors.Join(ErrInvalidRunDataIdentity, err)
		}
		if err != nil || !observed {
			n.Close()
			result = nil
		}
	}()
	ready, e := n.captureBase(ctx, opts.WaitForInitialDirectory)
	if e != nil || !ready {
		return nil, false, e
	}
	var runs *os.Root
	for {
		lock, state, e := acquireStreamMetadataReadAt(ctx, id, n.physical)
		if e != nil {
			return nil, false, e
		}
		capture := func() (bool, error) {
			if e := n.validateRoots(); e != nil {
				return false, e
			}
			if runs == nil {
				r, found, e := n.addChild(n.base, "runs")
				if e != nil {
					return false, e
				}
				if found {
					runs = r
				}
			}
			if runs == nil {
				return false, nil
			}
			if n.run == nil {
				r, found, e := n.addChild(runs, id)
				if e != nil {
					return false, e
				}
				if !found {
					return false, nil
				}
				n.run = r
			}
			h, raw, present, e := streamHeader(ctx, n.run, id)
			if e != nil {
				return false, e
			}
			if !present {
				if _, bound := RunDataBindingFrom(ctx); bound {
					return false, ErrRunNotFound
				}
				n.present = false
				n.observation, e = CaptureRunObservation(RunDataHeader{RunID: id, Format: LegacyValueFormat}, n.tenant, false)
				if e != nil {
					return false, e
				}
				n.domain = n.run
				n.watch = filepath.Join(n.physical, "runs", id)
				return true, nil
			}
			n.domain = nil
			n.pending = nil
			n.remaining = nil
			n.present = true
			n.header = h
			tenant, e := streamCanonicalTenant(ctx, raw)
			if e != nil {
				if filesystemContextErrorOnly(e) {
					return false, e
				}
				return false, errors.Join(ErrInvalidRunDataIdentity, e, ctx.Err())
			}
			// streamHeader compared the header; the tenant parsed from the
			// same bytes is compared with the one the caller's binding observed.
			if e = CheckRunObservedTenant(ctx, tenant); e != nil {
				return false, e
			}
			n.observation, e = CaptureRunObservation(h, tenant, true)
			if e != nil {
				return false, e
			}
			n.binding, e = CaptureRunDataBinding(h)
			if e != nil {
				return false, e
			}
			bound, e := WithRunDataBinding(ctx, n.binding)
			if e != nil {
				return false, e
			}
			if h.Protocol == HistoricalRunData {
				n.domain = n.run
				n.watch = filepath.Join(n.physical, "runs", id)
				return true, nil
			}
			if !runBirthAtomicPublicationSupported() {
				return false, ErrRunDataCapability
			}
			scope, e := ResolveIncarnationDataScope(bound, h)
			if e != nil {
				return false, e
			}
			parts, e := scope.components()
			if e != nil {
				return false, e
			}
			n.pending = n.base
			n.remaining = append([]string{".run-data-v1"}, parts...)
			n.watch, e = scope.FilesystemRoot(n.physical)
			if e != nil {
				return false, e
			}
			if e = n.ensureDomain(); e != nil {
				return false, e
			}
			return true, nil
		}
		ready, e := capture()
		if lock != nil {
			e = errors.Join(e, lock.Unlock())
		}
		if state == streamReadLockQuiescent && ctx.Err() == nil {
			// A provisional initial header is not published until the final lock
			// probe. An appearing writer must finish before initial capture repeats.
			after, afterState, ae := acquireStreamMetadataReadAt(ctx, id, n.physical)
			if ae != nil {
				e = errors.Join(e, ae)
			} else if afterState == streamReadLockOwned {
				ready, e = capture()
				e = errors.Join(e, after.Unlock())
			} else if n.run != nil {
				// Quiescence is not ownership. Initial metadata was provisional:
				// recapture after this final probe in a root already retained before it.
				// A nil run root instead keeps the initial wait/empty-log behavior.
				ready, e = capture()
			}
		}
		if e != nil {
			return nil, false, e
		}
		if ready {
			return n, true, nil
		}
		if _, bound := RunDataBindingFrom(ctx); bound {
			return nil, false, ErrRunNotFound
		}
		if !opts.WaitForInitialDirectory {
			return nil, false, nil
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, false, ctx.Err()
		case <-timer.C:
		}
	}
}

// captureBase retains the nearest existing configured ancestor before an
// initial wait. No run observation exists until its directory/header is seen.
func (n *filesystemStreamSelection) captureBase(ctx context.Context, wait bool) (bool, error) {
	path, e := filepath.Abs(n.s.root)
	if e != nil {
		return false, e
	}
	var missing []string
	resolvedAliases := make(map[string]bool)
	for {
		physical, err := filepath.EvalSymlinks(path)
		if err == nil {
			n.physical = physical
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			// EvalSymlinks words a link loop its own way: the kernel's answer
			// for the same path classifies it.
			if _, statErr := os.Stat(path); statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
				err = errors.Join(err, statErr)
			}
			return false, streamNodeError(err)
		}
		// Configuration aliases are trusted, including initially dangling ones.
		// Resolve their target once before retaining the nearest existing ancestor.
		// Descendants of that retained root still use no-follow streamChild.
		if info, statErr := os.Lstat(path); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			if resolvedAliases[path] {
				return false, ErrInvalidRunDataIdentity
			}
			resolvedAliases[path] = true
			target, readErr := os.Readlink(path)
			if readErr != nil {
				return false, readErr
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(path), target)
			}
			path = filepath.Clean(target)
			continue
		} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return false, statErr
		}
		parent := filepath.Dir(path)
		if parent == path {
			return false, err
		}
		missing = append([]string{filepath.Base(path)}, missing...)
		path = parent
	}
	n.baseInfo, e = os.Stat(n.physical)
	if e != nil {
		return false, e
	}
	if !n.baseInfo.IsDir() {
		return false, ErrRunDataIdentityChanged
	}
	n.base, e = os.OpenRoot(n.physical)
	if e != nil {
		return false, e
	}
	n.roots = append(n.roots, n.base)
	info, e := n.base.Stat(".")
	if e != nil {
		return false, streamNodeError(e)
	}
	if !os.SameFile(n.baseInfo, info) {
		return false, ErrRunDataIdentityChanged
	}
	initial := n.physical
	parts := append([]string(nil), missing...)
	for len(missing) > 0 {
		if e = ctx.Err(); e != nil {
			return false, e
		}
		if e = n.validateRoots(); e != nil {
			return false, e
		}
		child, found, e := n.addChild(n.base, missing[0])
		if e != nil {
			return false, e
		}
		if found {
			n.base = child
			missing = missing[1:]
			continue
		}
		if _, bound := RunDataBindingFrom(ctx); bound {
			return false, ErrRunNotFound
		}
		if !wait {
			return false, nil
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-timer.C:
		}
	}
	n.physical = filepath.Join(append([]string{initial}, parts...)...)
	n.baseInfo, e = n.base.Stat(".")
	return e == nil, e
}

func (n *filesystemStreamSelection) validateRoots() error {
	current, e := os.Stat(n.physical)
	if e != nil {
		return streamNodeError(e)
	}
	if !os.SameFile(n.baseInfo, current) {
		return ErrRunDataIdentityChanged
	}
	for _, link := range n.links {
		current, e := link.parent.Lstat(link.name)
		if e != nil {
			return streamNodeError(e)
		}
		if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(link.info, current) {
			return ErrRunDataIdentityChanged
		}
	}
	return nil
}
func (n *filesystemStreamSelection) ensureDomain() error {
	if n.domain != nil {
		return nil
	}
	for len(n.remaining) > 0 {
		root, found, e := n.addChild(n.pending, n.remaining[0])
		if e != nil {
			return e
		}
		if !found {
			return nil
		}
		n.pending = root
		n.remaining = n.remaining[1:]
	}
	n.domain = n.pending
	return nil
}
func (n *filesystemStreamSelection) operation(ctx context.Context) (context.Context, error) {
	if ctx == nil {
		return nil, ErrInvalidRunDataIdentity
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if n.closed {
		return nil, ErrRunDataCapability
	}
	if tenant, ok := TenantFromContext(ctx); ok && tenant != n.tenant {
		return nil, ErrRunDataIdentityChanged
	}
	if owner, ok := OwnerFromContext(ctx); ok && owner != n.owner {
		return nil, ErrRunDataIdentityChanged
	}
	if IsWithoutTenantFilter(ctx) && !n.privileged {
		return nil, ErrRunDataIdentityChanged
	}
	ctx = WithIdentity(ctx, n.tenant, n.owner)
	if n.privileged {
		ctx = WithoutTenantFilter(ctx)
	}
	if _, e := ValueFormatForWrite(ctx, n.id); e != nil {
		return nil, e
	}
	if n.present {
		b, e := WithRunDataBinding(ctx, n.binding)
		if e != nil {
			return nil, e
		}
		return WithRunValueFormat(b, RunValueFormat{RunID: n.id, Format: n.header.Format})
	}
	if _, bound := RunDataBindingFrom(ctx); bound {
		return nil, ErrRunDataIdentityChanged
	}
	if n.hasIntent {
		if incoming, ok := ctx.Value(runValueFormatKey{}).(RunValueFormat); ok && incoming != n.intent {
			return nil, ErrValueFormatChanged
		}
		return WithRunValueFormat(ctx, n.intent)
	}
	return ctx, nil
}
func (n *filesystemStreamSelection) check(ctx context.Context) (context.Context, []byte, error) {
	ctx, e := n.operation(ctx)
	if e != nil {
		return nil, nil, e
	}
	// The lock path goes through the retained roots: they are checked first,
	// so a replaced ancestor is refused, not retried as a lock.
	if e = n.validateRoots(); e != nil {
		return nil, nil, e
	}
	lock, state, e := acquireStreamMetadataReadAt(ctx, n.id, n.physical)
	if e != nil {
		return nil, nil, e
	}
	read := func() ([]byte, error) {
		if e := n.validateRoots(); e != nil {
			return nil, e
		}
		h, raw, present, e := streamHeader(ctx, n.run, n.id)
		if e != nil {
			return nil, e
		}
		tenant := ""
		if present {
			tenant, e = streamCanonicalTenant(ctx, raw)
			if e != nil {
				if filesystemContextErrorOnly(e) {
					return nil, e
				}
				return nil, errors.Join(ErrInvalidRunDataIdentity, e, ctx.Err())
			}
		}
		if present != n.present || present && (h != n.header || tenant != n.observation.TenantID()) {
			return nil, ErrRunDataIdentityChanged
		}
		// The operation's ctx may carry an observed tenant the open never saw.
		if present {
			if e = CheckRunObservedTenant(ctx, tenant); e != nil {
				return nil, e
			}
		}
		if e = n.validateRoots(); e != nil {
			return nil, e
		}
		return raw, ctx.Err()
	}
	raw, e := read()
	if lock != nil {
		e = errors.Join(e, lock.Unlock())
	}
	if state == streamReadLockQuiescent && ctx.Err() == nil {
		after, afterState, ae := acquireStreamMetadataReadAt(ctx, n.id, n.physical)
		if ae != nil {
			return nil, nil, errors.Join(e, ae)
		}
		if afterState == streamReadLockOwned {
			raw, e = read()
			e = errors.Join(e, after.Unlock())
		} else if e == nil {
			raw, e = read()
		}
	}
	if e != nil {
		return nil, nil, e
	}
	return ctx, raw, ctx.Err()
}
func (n *filesystemStreamSelection) Validate(ctx context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, _, e := n.check(ctx)
	return e
}
func (n *filesystemStreamSelection) Terminal(ctx context.Context) (bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	ctx, raw, e := n.check(ctx)
	if e != nil {
		return false, e
	}
	if !n.present {
		return false, nil
	}
	var run Run
	if e = DecodeValueRecordJSON(ctx, n.header.Format, ExactRun, raw, &run); e != nil {
		if filesystemContextErrorOnly(e) {
			return false, e
		}
		return false, errors.Join(ErrEventsCorrupted, e, ctx.Err())
	}
	return run.Status.IsTerminal(), nil
}
func (n *filesystemStreamSelection) open(ctx context.Context, name string, structured bool) (*os.File, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	ctx, _, e := n.check(ctx)
	if e != nil {
		return nil, e
	}
	if structured && !n.present {
		if _, e = CheckRunValueFormat(ctx, n.id, 0); e != nil {
			return nil, e
		}
	}
	if e = n.ensureDomain(); e != nil {
		return nil, e
	}
	if n.domain == nil {
		return nil, os.ErrNotExist
	}
	f, _, e := openStreamRegular(n.domain, name)
	if e != nil {
		return nil, e
	}
	if e = ctx.Err(); e != nil {
		f.Close()
		return nil, e
	}
	return f, nil
}
func (n *filesystemStreamSelection) OpenEvents(ctx context.Context) (*os.File, error) {
	return n.open(ctx, "events.jsonl", true)
}
func (n *filesystemStreamSelection) OpenLog(ctx context.Context) (*os.File, error) {
	return n.open(ctx, "run.log", false)
}
func (n *filesystemStreamSelection) CheckEventObservationSelection(actual any) error {
	if n == nil || actual != n {
		return ErrRunDataCapability
	}
	return nil
}
func (n *filesystemStreamSelection) EventStreamObservation() RunObservation { return n.observation }
func (n *filesystemStreamSelection) DecodeObservedEvent(ctx context.Context, raw []byte) (*Event, error) {
	evt, err := n.DecodeEvent(ctx, raw)
	if err != nil {
		return nil, err
	}
	if evt.RunID == "" {
		evt.RunID = n.id
	}
	stamped, err := WithEventObservation(*evt, n.observation)
	if err != nil {
		return nil, err
	}
	return &stamped, nil
}
func (n *filesystemStreamSelection) DecodeEvent(ctx context.Context, raw []byte) (*Event, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	ctx, e := n.operation(ctx)
	if e != nil {
		return nil, e
	}
	format := LegacyValueFormat
	if n.present {
		format = n.header.Format
	} else {
		if _, e = CheckRunValueFormat(ctx, n.id, 0); e != nil {
			return nil, e
		}
	}
	var evt Event
	if e = decodeStreamEventJSON(ctx, format, raw, &evt); e != nil {
		if format == ExactValueFormat {
			if filesystemContextErrorOnly(e) {
				return nil, e
			}
			return nil, errors.Join(ErrEventsCorrupted, e, ctx.Err())
		}
		return nil, e
	}
	if evt.RunID != "" && evt.RunID != n.id {
		return nil, ErrRunDataIdentityChanged
	}
	return &evt, nil
}

// decodeStreamEventJSON decodes one persisted event line for a filesystem
// stream. A legacy-format line keeps its numbers as written (json.Number):
// streamed events are re-encoded for their consumers, never through float64,
// so an integer past 2^53 stays exact and a number past the float64 range is
// delivered instead of skipped as undecodable. The exact format has its own
// codec. Other legacy readers keep DecodeValueRecordJSON's float64 numbers.
func decodeStreamEventJSON(ctx context.Context, f ValueFormat, raw []byte, dst *Event) error {
	f, err := NormalizeValueFormat(int(f))
	if err != nil {
		return err
	}
	if f == ExactValueFormat {
		return DecodeValueRecordJSON(ctx, f, ExactEvent, raw, dst)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value Event
	if err = dec.Decode(&value); err != nil {
		return err
	}
	if _, err = dec.Token(); err != io.EOF {
		if err == nil {
			err = errors.New("store: data after the event record")
		}
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	*dst = value
	return nil
}
func (n *filesystemStreamSelection) WatchHints() (string, string) { return n.watch, n.watch }
func (n *filesystemStreamSelection) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil
	}
	n.closed = true
	var errs []error
	for i := len(n.roots) - 1; i >= 0; i-- {
		errs = append(errs, n.roots[i].Close())
	}
	n.roots = nil
	return errors.Join(errs...)
}

// A cancellation leaf must not hide deterministic failures in joined errors.
func filesystemContextErrorOnly(err error) bool {
	if err == nil {
		return false
	}
	if many, ok := err.(interface{ Unwrap() []error }); ok {
		parts := many.Unwrap()
		if len(parts) == 0 {
			return false
		}
		for _, part := range parts {
			if !filesystemContextErrorOnly(part) {
				return false
			}
		}
		return true
	}
	if one, ok := err.(interface{ Unwrap() error }); ok {
		return filesystemContextErrorOnly(one.Unwrap())
	}
	return err == context.Canceled || err == context.DeadlineExceeded
}
