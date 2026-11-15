package runstream

import (
	"bufio"
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"io"
	"math"
	"os"
)

func isFileMissing(e error) bool {
	return e != nil && !IsPermanentError(e) && errors.Is(e, os.ErrNotExist)
}

type fileDrain struct {
	selection       store.FilesystemStreamSelection
	original        store.RunObservation
	observed        bool
	admissionErr    error
	observedDecoder store.ObservedFilesystemEventDecoder
	file            *os.File
	info            os.FileInfo
	offset          int64
	seq             int64
	after           bool
	delivered       int64
	logs            bool
	events          EventPipe
	logpipe         LogPipe
	scratch         []byte
	skipped, valid  int
}

func newFileEventDrain(s store.FilesystemStreamSelection, p EventPipe, from int64) *fileDrain {
	d := &fileDrain{selection: s, events: p, seq: from}
	d.original, d.observed, d.admissionErr = store.CaptureEventStreamObservation(s)
	if d.observed {
		var ok bool
		d.observedDecoder, ok = s.(store.ObservedFilesystemEventDecoder)
		if !ok {
			d.admissionErr = store.ErrRunDataCapability
		}
	}
	return d
}
func newFileLogDrain(s store.FilesystemStreamSelection, p LogPipe, from int64) *fileDrain {
	if from < 0 {
		from = 0
	}
	return &fileDrain{selection: s, logpipe: p, logs: true, delivered: from, scratch: make([]byte, logChunkBudget)}
}
func (d *fileDrain) Close() error {
	if d.file != nil {
		e := d.file.Close()
		d.file = nil
		return e
	}
	return nil
}
func (d *fileDrain) open(ctx context.Context) (bool, error) {
	var file *os.File
	var e error
	if d.logs {
		file, e = d.selection.OpenLog(ctx)
	} else {
		file, e = d.selection.OpenEvents(ctx)
	}
	if isFileMissing(e) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	if file == nil {
		return false, store.ErrRunDataCapability
	}
	info, e := file.Stat()
	if e != nil {
		file.Close()
		return false, e
	}
	rotated := d.info != nil && !os.SameFile(info, d.info)
	if d.file == nil || rotated {
		if d.file != nil {
			d.file.Close()
		}
		d.file = file
		d.offset = 0
		if rotated {
			d.skipped, d.valid = 0, 0
			if d.logs {
				d.delivered = 0
			}
		}
	} else {
		file.Close()
	}
	if info.Size() < d.offset {
		d.offset = 0
		d.skipped, d.valid = 0, 0
		if d.logs {
			d.delivered = 0
		}
	}
	d.info = info
	return true, nil
}
func (d *fileDrain) Drain(ctx context.Context) error {
	if d.admissionErr != nil {
		return d.admissionErr
	}
	ready, e := d.open(ctx)
	if e != nil || !ready {
		return e
	}
	if e = d.selection.Validate(ctx); e != nil {
		return e
	}
	end := d.info.Size()
	if end < 0 || d.offset < 0 {
		return store.ErrEventsCorrupted
	}
	if _, e = d.file.Seek(d.offset, io.SeekStart); e != nil {
		return e
	}
	reader := io.LimitReader(d.file, end-d.offset)
	if d.logs {
		return d.drainLogs(ctx, reader)
	}
	return d.drainEvents(ctx, reader)
}
func (d *fileDrain) drainLogs(ctx context.Context, reader io.Reader) error {
	for {
		if e := ctx.Err(); e != nil {
			return e
		}
		n, e := reader.Read(d.scratch)
		if n > 0 {
			if int64(n) > math.MaxInt64-d.offset {
				return store.ErrEventsCorrupted
			}
			if e2 := d.selection.Validate(ctx); e2 != nil {
				return e2
			}
			chunk := append([]byte(nil), d.scratch[:n]...)
			next, ok := ShipLogChunk(ctx, d.logpipe, d.offset, chunk, d.delivered)
			if !ok {
				return ctx.Err()
			}
			d.delivered = next
			d.offset += int64(n)
		}
		if e != nil {
			if errors.Is(e, io.EOF) {
				return nil
			}
			return e
		}
		if n == 0 {
			return nil
		}
	}
}
func (d *fileDrain) drainEvents(ctx context.Context, reader io.Reader) error {
	r := bufio.NewReader(reader)
	batch := make([]*store.Event, 0, min(MaxEventsPerPage(), 256))
	offset := d.offset
	flush := func() error {
		if len(batch) > 0 {
			if e := d.selection.Validate(ctx); e != nil {
				return e
			}
			if e := checkObservedBatch(batch, d.original, d.observed); e != nil {
				return e
			}
			if !d.events.Ship(ctx, batch) {
				return eventShipRefused(d.events, ctx)
			}
			d.seq = batch[len(batch)-1].Seq
			d.after = true
			batch = nil
		}
		d.offset = offset
		return nil
	}
	for {
		if e := ctx.Err(); e != nil {
			return e
		}
		line, e := r.ReadBytes('\n')
		if len(line) > 0 {
			if line[len(line)-1] != '\n' {
				return flush()
			}
			if int64(len(line)) > math.MaxInt64-offset {
				return store.ErrEventsCorrupted
			}
			offset += int64(len(line))
			raw := line[:len(line)-1]
			if len(raw) > 0 {
				var evt *store.Event
				var de error
				if d.observed {
					evt, de = d.observedDecoder.DecodeObservedEvent(ctx, raw)
				} else {
					evt, de = d.selection.DecodeEvent(ctx, raw)
				}
				if de != nil {
					if IsPermanentError(de) || ctx.Err() != nil {
						return de
					}
					d.skipped++
					d.events.Warn(de)
				} else if evt != nil {
					d.valid++
					if evt.Seq > d.seq || evt.Seq == d.seq && !d.after {
						batch = append(batch, evt)
					}
				} else {
					return store.ErrRunDataCapability
				}
			}
			if len(batch) >= MaxEventsPerPage() {
				if e := flush(); e != nil {
					return e
				}
			}
		}
		if e != nil {
			if !errors.Is(e, io.EOF) {
				return e
			}
			if e = flush(); e != nil {
				return e
			}
			return store.CheckEventStreamCorruption(d.skipped, d.valid)
		}
	}
}
