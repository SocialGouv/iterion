package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/spec"
	"github.com/SocialGouv/iterion/pkg/store"
)

type portOutputArea struct {
	HostDir    string
	SandboxDir string
}

func needsPortOutputArea(instance *ir.PortInstance) bool {
	for _, port := range instance.Contract.Outputs {
		if port.Type.Name == "file" {
			return true
		}
	}
	return false
}

// Every attempt gets a new directory inside the already-mounted native
// scratch root. Mkdir (not MkdirAll for the attempt itself) refuses a stale
// directory if a crashed admission somehow tries the same attempt again.
func (e *Engine) preparePortOutputArea(ctx context.Context, runID string, invocation *store.PortInvocation) (*portOutputArea, error) {
	files := store.AsRunFilesStore(e.store)
	if files == nil || store.AsPortFilesStore(e.store) == nil {
		return nil, fmt.Errorf("runtime: native file output requires run-files and immutable capture stores")
	}
	if err := store.SanitizePathComponent("invocation", invocation.ID); err != nil {
		return nil, err
	}
	rootDir, err := files.EnsureRunFilesDir(ctx, runID)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	parent := filepath.Join("invocations", invocation.ID)
	if err := root.MkdirAll(parent, 0o775); err != nil {
		return nil, err
	}
	name := filepath.Join(parent, strconv.Itoa(invocation.Attempt))
	if err := root.Mkdir(name, 0o775); err != nil {
		return nil, fmt.Errorf("runtime: invocation %s attempt %d does not own a fresh output directory: %w", invocation.ID, invocation.Attempt, err)
	}
	if err := root.Mkdir(filepath.Join(name, "outputs"), 0o775); err != nil {
		return nil, err
	}
	for _, dir := range []string{parent, name, filepath.Join(name, "outputs")} {
		f, err := root.Open(dir)
		if err != nil {
			return nil, err
		}
		if err := f.Chmod(0o775); err != nil {
			_ = f.Close()
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
	}
	return &portOutputArea{
		HostDir:    filepath.Join(rootDir, name, "outputs"),
		SandboxDir: path.Join("/iterion/artifact-files", filepath.ToSlash(name), "outputs"),
	}, nil
}

func (a *portOutputArea) relativeOutputPath(candidate any) (string, error) {
	var supplied string
	switch value := candidate.(type) {
	case string:
		supplied = value
	case map[string]any:
		supplied, _ = value["path"].(string)
	}
	if supplied == "" || strings.Contains(supplied, "\\") {
		return "", fmt.Errorf("runtime: file output needs a path under its invocation output directory")
	}
	for _, base := range []string{a.HostDir, a.SandboxDir} {
		if strings.HasPrefix(supplied, base+string(os.PathSeparator)) || strings.HasPrefix(supplied, base+"/") {
			supplied = strings.TrimPrefix(supplied, base)
			supplied = strings.TrimLeft(supplied, "/")
			break
		}
	}
	if !filepath.IsLocal(supplied) || supplied == "." || strings.Contains(supplied, "..") {
		return "", fmt.Errorf("runtime: file output path escapes the current invocation")
	}
	return supplied, nil
}

func (e *Engine) capturePortFile(ctx context.Context, runID string, invocation *store.PortInvocation, port ir.PublicPort, candidate any, area *portOutputArea) (map[string]any, store.PortFileRef, error) {
	if area == nil {
		return nil, store.PortFileRef{}, fmt.Errorf("runtime: file output %s.%s has no fresh invocation area", invocation.Node, port.Name)
	}
	rel, err := area.relativeOutputPath(candidate)
	if err != nil {
		return nil, store.PortFileRef{}, err
	}
	root, err := os.OpenRoot(area.HostDir)
	if err != nil {
		return nil, store.PortFileRef{}, err
	}
	defer root.Close()
	f, err := root.Open(rel)
	if err != nil {
		return nil, store.PortFileRef{}, fmt.Errorf("runtime: declared file %s.%s was not produced: %w", invocation.Node, port.Name, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, store.PortFileRef{}, err
	}
	if !info.Mode().IsRegular() {
		return nil, store.PortFileRef{}, fmt.Errorf("runtime: declared file %s.%s is not regular", invocation.Node, port.Name)
	}
	if port.File != nil && info.Size() < port.File.MinBytes {
		return nil, store.PortFileRef{}, fmt.Errorf("runtime: declared file %s.%s has %d bytes, below required %d", invocation.Node, port.Name, info.Size(), port.File.MinBytes)
	}
	header := make([]byte, 512)
	n, err := f.Read(header)
	if err != nil && err != io.EOF {
		return nil, store.PortFileRef{}, err
	}
	header = header[:n]
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, store.PortFileRef{}, err
	}
	mediaType, _, _ := mime.ParseMediaType(http.DetectContentType(header))
	if port.File != nil && port.File.MediaType != "" {
		mediaType = port.File.MediaType
	}
	if mediaType == "application/json" || port.File != nil && port.File.Schema != nil {
		const maxSchemaFileBytes = 32 << 20
		if info.Size() > maxSchemaFileBytes {
			return nil, store.PortFileRef{}, fmt.Errorf("runtime: JSON file %s.%s exceeds the validation limit", invocation.Node, port.Name)
		}
		body, err := io.ReadAll(io.LimitReader(f, maxSchemaFileBytes+1))
		if err != nil {
			return nil, store.PortFileRef{}, err
		}
		if int64(len(body)) != info.Size() {
			return nil, store.PortFileRef{}, fmt.Errorf("runtime: file changed during validation")
		}
		value, err := spec.DecodePublicJSON(body)
		if err != nil {
			return nil, store.PortFileRef{}, fmt.Errorf("runtime: declared JSON file %s.%s is invalid: %w", invocation.Node, port.Name, err)
		}
		if port.File != nil && port.File.Schema != nil {
			typeOfFile := ir.PortType{Name: port.File.Schema.Name, Schema: port.File.Schema, ShapeHash: port.File.ShapeHash}
			if err := typeOfFile.ValidateValue(value); err != nil {
				return nil, store.PortFileRef{}, fmt.Errorf("runtime: declared file schema %s.%s: %w", invocation.Node, port.Name, err)
			}
		}
	} else if port.File != nil && port.File.MediaType != "" {
		observed, _, _ := mime.ParseMediaType(http.DetectContentType(header))
		if mediaType == "text/plain" {
			if observed != "text/plain" || !utf8.Valid(header) {
				return nil, store.PortFileRef{}, fmt.Errorf("runtime: declared text file %s.%s has incompatible bytes", invocation.Node, port.Name)
			}
		} else if observed != mediaType {
			return nil, store.PortFileRef{}, fmt.Errorf("runtime: declared media type %s for %s.%s cannot be verified (detected %s)", mediaType, invocation.Node, port.Name, observed)
		}
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, store.PortFileRef{}, err
	}
	hash := sha256.New()
	size, err := io.Copy(hash, f)
	if err != nil {
		return nil, store.PortFileRef{}, err
	}
	if size != info.Size() {
		return nil, store.PortFileRef{}, fmt.Errorf("runtime: file changed during hashing")
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	refPath, err := store.PortFilePath(invocation.ID, invocation.Attempt, digest)
	if err != nil {
		return nil, store.PortFileRef{}, err
	}
	ref := store.PortFileRef{RunID: runID, Path: refPath, SHA256: digest, Size: size, MediaType: mediaType, Producer: invocation.ID, Attempt: invocation.Attempt}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, store.PortFileRef{}, err
	}
	if err := store.AsPortFilesStore(e.store).PutPortFile(ctx, ref, f); err != nil {
		return nil, store.PortFileRef{}, err
	}
	return portFileDescriptor(ref), ref, nil
}

// A file passed through from a committed input keeps its original immutable
// reference. New files must come from this invocation's fresh output area.
// Both branches return canonical descriptors; executor-supplied metadata is
// never treated as evidence of a published file.
func (e *Engine) capturePortFiles(ctx context.Context, runID string, invocation *store.PortInvocation, port ir.PublicPort, candidate any, area *portOutputArea, state *store.PortExecution) (any, []store.PortFileRef, error) {
	var capture func(any, int) (any, []store.PortFileRef, error)
	capture = func(value any, depth int) (any, []store.PortFileRef, error) {
		if value == nil && port.Nullable && depth == port.Type.ArrayDepth {
			return nil, nil, nil
		}
		if depth > 0 {
			array := reflect.ValueOf(value)
			if !array.IsValid() || array.Kind() != reflect.Slice && array.Kind() != reflect.Array {
				return nil, nil, fmt.Errorf("runtime: expected array of file outputs for %s.%s", invocation.Node, port.Name)
			}
			out := make([]any, array.Len())
			var refs []store.PortFileRef
			for i := range out {
				item, files, err := capture(array.Index(i).Interface(), depth-1)
				if err != nil {
					return nil, nil, fmt.Errorf("runtime: file output %s.%s item %d: %w", invocation.Node, port.Name, i, err)
				}
				out[i], refs = item, append(refs, files...)
			}
			return out, refs, nil
		}
		path := ""
		switch v := value.(type) {
		case string:
			path = v
		case map[string]any:
			path, _ = v["path"].(string)
		}
		for _, revision := range invocation.Inputs {
			publication := state.Publications[revision]
			if publication == nil {
				continue
			}
			for _, ref := range publication.Files {
				if ref.Path != path {
					continue
				}
				if port.File != nil && (ref.Size < port.File.MinBytes || port.File.MediaType != "" && ref.MediaType != port.File.MediaType) {
					return nil, nil, fmt.Errorf("runtime: passed-through file does not satisfy %s.%s", invocation.Node, port.Name)
				}
				body, _, err := store.AsRunFilesStore(e.store).OpenRunFile(ctx, ref.RunID, ref.Path)
				if err != nil {
					return nil, nil, err
				}
				verifyErr := store.VerifyPortFile(ctx, ref, body)
				verifyErr = errors.Join(verifyErr, body.Close())
				if verifyErr != nil {
					return nil, nil, verifyErr
				}
				if port.File != nil && port.File.Schema != nil {
					const maxSchemaFileBytes = 32 << 20
					if ref.Size > maxSchemaFileBytes {
						return nil, nil, fmt.Errorf("runtime: passed-through JSON file exceeds validation limit")
					}
					body, _, err := store.AsRunFilesStore(e.store).OpenRunFile(ctx, ref.RunID, ref.Path)
					if err != nil {
						return nil, nil, err
					}
					raw, readErr := io.ReadAll(io.LimitReader(body, maxSchemaFileBytes+1))
					if err := errors.Join(readErr, body.Close()); err != nil {
						return nil, nil, err
					}
					if int64(len(raw)) != ref.Size {
						return nil, nil, fmt.Errorf("runtime: passed-through JSON file changed during validation")
					}
					decoded, err := spec.DecodePublicJSON(raw)
					if err != nil {
						return nil, nil, err
					}
					typeOfFile := ir.PortType{Name: port.File.Schema.Name, Schema: port.File.Schema, ShapeHash: port.File.ShapeHash}
					if err := typeOfFile.ValidateValue(decoded); err != nil {
						return nil, nil, err
					}
				}
				return portFileDescriptor(ref), []store.PortFileRef{ref}, nil
			}
		}
		descriptor, ref, err := e.capturePortFile(ctx, runID, invocation, port, value, area)
		if err != nil {
			return nil, nil, err
		}
		return descriptor, []store.PortFileRef{ref}, nil
	}
	return capture(candidate, port.Type.ArrayDepth)
}

func portFileDescriptor(ref store.PortFileRef) map[string]any {
	return map[string]any{"path": ref.Path, "sha256": ref.SHA256, "size": ref.Size, "media_type": ref.MediaType}
}
