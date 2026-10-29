package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/internal/yamlx"
	yaml "gopkg.in/yaml.v3"
)

// File names inside a connector package directory.
const (
	ConnectorFile = "connector.yaml"
	SchemasFile   = "schemas.yaml"
	ResponsesFile = "responses.json"
	TriggersFile  = "triggers.yaml"
	OpsDir        = "ops"
)

// Write materialises a package as a directory. Every file is deterministic —
// sorted maps, sorted operations — so regenerating an unchanged description
// produces byte-identical output and a diff shows only real change. That is
// what makes a generated package reviewable in a PR rather than a blob.
func Write(dir string, p *Package) error {
	if err := p.ValidateResponseContracts(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, OpsDir), 0o755); err != nil {
		return fmt.Errorf("spec: create %s: %w", dir, err)
	}
	if err := writeYAML(filepath.Join(dir, ConnectorFile), p.Connector); err != nil {
		return err
	}
	if len(p.Schemas) > 0 {
		sf := schemasDoc{SchemaVersion: p.Connector.SchemaVersion, Connector: p.Connector.ID, Schemas: p.Schemas}
		if err := writeYAML(filepath.Join(dir, SchemasFile), sf); err != nil {
			return err
		}
	}
	if len(p.ResponseSchemas) > 0 {
		// Stamped from the PACKAGE, never from a constant: a document that
		// names its own version independently of the package it sits in is a
		// second source of truth, and the day the format moves past 2 it would
		// mislabel every file it writes. ValidateResponseContracts above has
		// already refused a package too old to carry contracts at all.
		rf := responsesDoc{SchemaVersion: p.Connector.SchemaVersion, Connector: p.Connector.ID, Schemas: p.ResponseSchemas}
		body, err := json.MarshalIndent(rf, "", "  ")
		if err != nil {
			return fmt.Errorf("spec: encode %s: %w", ResponsesFile, err)
		}
		if len(body) > maxResponseContractBytes {
			return fmt.Errorf("spec: %s exceeds the contract size limit", ResponsesFile)
		}
		if err := os.WriteFile(filepath.Join(dir, ResponsesFile), append(body, '\n'), 0o644); err != nil {
			return fmt.Errorf("spec: write %s: %w", ResponsesFile, err)
		}
	} else if err := os.Remove(filepath.Join(dir, ResponsesFile)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("spec: remove obsolete %s: %w", ResponsesFile, err)
	}
	for _, f := range p.Ops {
		name := f.Domain
		if name == "" {
			name = "default"
		}
		if err := writeYAML(filepath.Join(dir, OpsDir, name+".yaml"), f); err != nil {
			return err
		}
	}
	return nil
}

// schemasDoc is the on-disk shape of schemas.yaml (SchemasFile is taken by
// the file-name constant).
type schemasDoc struct {
	SchemaVersion int               `yaml:"schema_version"`
	Connector     string            `yaml:"connector"`
	Schemas       map[string]Schema `yaml:"schemas"`
}

type responsesDoc struct {
	SchemaVersion int                       `json:"schema_version"`
	Connector     string                    `json:"connector"`
	Schemas       map[string]ResponseSchema `json:"schemas"`
}

const maxResponseContractBytes = 4 << 20

func writeYAML(path string, v any) error {
	body, err := yamlx.Marshal(v)
	if err != nil {
		return fmt.Errorf("spec: encode %s: %w", path, err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return fmt.Errorf("spec: write %s: %w", path, err)
	}
	return nil
}

// versionProbe is the tolerant PRE-PASS: it reads schema_version and ignores
// every other key, so a package written for a newer iterion is diagnosed as
// "upgrade iterion" instead of failing inside the strict decoder on whichever
// unknown field happens to come first.
//
// The order is the whole point. pkg/plugin does the strict decode FIRST and
// its own "schema_version newer than supported" message is consequently
// unreachable — a defect worth not repeating in a format that is designed to
// grow.
type versionProbe struct {
	SchemaVersion int `yaml:"schema_version"`
}

// DirFS is the directory dir as an fs.FS, with dir restored in the paths its
// errors name.
//
// The loaders below read through io/fs so that ONE of them serves a directory
// on disk, a package held anywhere else, and the packages compiled into the
// binary. A second loader for the embedded copy is the defect
// pkg/connection's catalog comment describes: the merged package was read by
// one path and the generated half by another, and the two disagreed about
// every correction the authored half exists to make.
//
// The wrapper, rather than os.DirFS bare, because os.DirFS rewrites a
// *fs.PathError's Path to the name RELATIVE to its root: "read connector.yaml:
// open connector.yaml: no such file or directory" sends an operator looking in
// their own working directory for a file that belongs to a package somewhere
// else. Restoring the root is what keeps every path caller's message naming a
// file that can be opened.
//
// An EMPTY dir is refused by os.DirFS itself rather than read as the process's
// working directory — the one difference from joining paths by hand, and the
// right one: a package has a directory, and serving whatever the cwd happens
// to hold is a reading nobody asks for (the project tier exists because that
// directory is untrusted).
func DirFS(dir string) fs.FS { return dirFS{dir: dir, fsys: os.DirFS(dir)} }

type dirFS struct {
	dir  string
	fsys fs.FS
}

func (d dirFS) Open(name string) (fs.File, error) {
	f, err := d.fsys.Open(name)
	return f, d.restore(err)
}

func (d dirFS) ReadFile(name string) ([]byte, error) {
	body, err := fs.ReadFile(d.fsys, name)
	return body, d.restore(err)
}

func (d dirFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(d.fsys, name)
	return entries, d.restore(err)
}

func (d dirFS) Stat(name string) (fs.FileInfo, error) {
	info, err := fs.Stat(d.fsys, name)
	return info, d.restore(err)
}

// restore puts the root back in front of the path the error names, in the
// operating system's own separator — what os.ReadFile would have reported.
func (d dirFS) restore(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		pe.Path = filepath.Join(d.dir, filepath.FromSlash(pe.Path))
	}
	return err
}

// Load reads a connector package from a directory and runs the COMPLETE
// check. It is the launch/publish path: what it returns is usable.
func Load(dir string) (*Package, error) { return LoadFS(DirFS(dir), ".") }

// LoadFS is Load against any filesystem: a directory (the form above), a
// package rebuilt from a store, the packages baked into the binary. dir is the
// package's own directory WITHIN fsys — "." when fsys is rooted at it.
func LoadFS(fsys fs.FS, dir string) (*Package, error) {
	p, err := readFS(fsys, dir)
	if err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// LoadGenerated reads a package and checks only what a generator guarantees.
//
// It exists because "generated, awaiting its overlay" is a real and expected
// state on disk — the generator writes it, the overlay merge reads it — and a
// loader that could not read that state would force the merge step to
// re-implement parsing. The merge calls Validate at the end; nothing else
// should use this.
func LoadGenerated(dir string) (*Package, error) { return LoadGeneratedFS(DirFS(dir), ".") }

// LoadGeneratedFS is LoadGenerated against any filesystem, for the reason
// LoadFS exists.
func LoadGeneratedFS(fsys fs.FS, dir string) (*Package, error) {
	p, err := readFS(fsys, dir)
	if err != nil {
		return nil, err
	}
	if err := p.ValidateGenerated(); err != nil {
		return nil, err
	}
	return p, nil
}

// CheckPackageDir refuses a package directory io/fs cannot address.
//
// Exported because every loader of a package — here, and the overlay half
// next door — has to apply it before joining anything onto it: an absolute
// path, or one climbing out with "..", is not a path within a filesystem, and
// fs implementations disagree about how they refuse it (fstest.MapFS answers
// "does not exist", which would read as "this package has no overlay" and
// silently serve an uncorrected package).
func CheckPackageDir(dir string) error {
	if !fs.ValidPath(dir) {
		return fmt.Errorf("spec: %q is not a package directory within a filesystem (io/fs takes a slash path with no leading slash and no \"..\"; pass a directory on disk through DirFS)", dir)
	}
	return nil
}

func readFS(fsys fs.FS, dir string) (*Package, error) {
	if err := CheckPackageDir(dir); err != nil {
		return nil, err
	}
	body, err := fs.ReadFile(fsys, path.Join(dir, ConnectorFile))
	if err != nil {
		return nil, fmt.Errorf("spec: read %s: %w", ConnectorFile, err)
	}
	var probe versionProbe
	if err := yaml.Unmarshal(body, &probe); err != nil {
		return nil, fmt.Errorf("spec: %s is not valid YAML: %w", ConnectorFile, err)
	}
	if probe.SchemaVersion > SchemaVersion {
		return nil, fmt.Errorf("spec: %s declares schema_version %d, newer than supported %d (upgrade iterion)", ConnectorFile, probe.SchemaVersion, SchemaVersion)
	}

	var c Connector
	if err := yamlx.UnmarshalStrict(body, &c); err != nil {
		return nil, fmt.Errorf("spec: parse %s: %w", ConnectorFile, err)
	}
	p := &Package{Connector: c, Schemas: map[string]Schema{}}

	if sb, err := fs.ReadFile(fsys, path.Join(dir, SchemasFile)); err == nil {
		if err := checkDocVersion(SchemasFile, sb); err != nil {
			return nil, err
		}
		var sf schemasDoc
		if err := yamlx.UnmarshalStrict(sb, &sf); err != nil {
			return nil, fmt.Errorf("spec: parse %s: %w", SchemasFile, err)
		}
		if err := checkDocConnector(SchemasFile, sf.Connector, c.ID); err != nil {
			return nil, err
		}
		p.Schemas = sf.Schemas
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("spec: read %s: %w", SchemasFile, err)
	}
	if err := readResponseContracts(fsys, dir, p); err != nil {
		return nil, err
	}

	opsDir := path.Join(dir, OpsDir)
	entries, err := fs.ReadDir(fsys, opsDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("spec: read %s: %w", OpsDir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		ob, err := fs.ReadFile(fsys, path.Join(opsDir, name))
		if err != nil {
			return nil, fmt.Errorf("spec: read %s/%s: %w", OpsDir, name, err)
		}
		where := OpsDir + "/" + name
		if err := checkDocVersion(where, ob); err != nil {
			return nil, err
		}
		var f OpsFile
		if err := yamlx.UnmarshalStrict(ob, &f); err != nil {
			return nil, fmt.Errorf("spec: parse %s: %w", where, err)
		}
		if err := checkDocConnector(where, f.Connector, c.ID); err != nil {
			return nil, err
		}
		p.Ops = append(p.Ops, f)
	}
	// LAST, because a trigger is checked against the operations: read here so
	// every loader gets the inbound half, since the half a caller forgets to
	// read is the half that goes silently inert.
	if err := readTriggers(fsys, dir, p); err != nil {
		return nil, err
	}
	return p, nil
}

func readResponseContracts(fsys fs.FS, dir string, p *Package) error {
	f, err := fsys.Open(path.Join(dir, ResponsesFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("spec: read %s: %w", ResponsesFile, err)
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(io.LimitReader(f, maxResponseContractBytes+1))
	if err != nil {
		return fmt.Errorf("spec: read %s: %w", ResponsesFile, err)
	}
	if len(body) > maxResponseContractBytes {
		return fmt.Errorf("spec: %s exceeds the contract size limit", ResponsesFile)
	}
	// Probe the version before strict fields, just like every other document.
	if err := checkDocVersion(ResponsesFile, body); err != nil {
		return err
	}
	if err := checkNoDuplicateJSONKeys(ResponsesFile, body); err != nil {
		return err
	}
	var rf responsesDoc
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rf); err != nil {
		return fmt.Errorf("spec: parse %s: %w", ResponsesFile, err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("spec: %s must contain one JSON document", ResponsesFile)
	}
	if p.Connector.SchemaVersion < ResponseContractsVersion {
		return fmt.Errorf("spec: %s is present but %s declares schema_version %d; response contracts require %d",
			ResponsesFile, ConnectorFile, p.Connector.SchemaVersion, ResponseContractsVersion)
	}
	// One package, one format. A document naming a different version than the
	// connector it sits beside means the two halves were written by different
	// iterions, and whichever is older silently lacks fields the other emits.
	if rf.SchemaVersion != p.Connector.SchemaVersion {
		return fmt.Errorf("spec: %s declares schema_version %d but %s declares %d",
			ResponsesFile, rf.SchemaVersion, ConnectorFile, p.Connector.SchemaVersion)
	}
	if err := checkDocConnector(ResponsesFile, rf.Connector, p.Connector.ID); err != nil {
		return err
	}
	p.ResponseSchemas = rf.Schemas
	return nil
}

// checkNoDuplicateJSONKeys refuses a document that names the same key twice in
// one object.
//
// encoding/json keeps the LAST value and says nothing, so a file holding
// `"Item"` twice loads as whichever copy came second — while a reviewer diffing
// the file sees both and has no way to tell which one runs. For a document
// whose whole job is to state what a vendor may answer, "what was reviewed" and
// "what is enforced" must be the same text.
//
// The token stream is the only place the repetition is still visible; by the
// time Decode returns, one of the two is gone.
func checkNoDuplicateJSONKeys(where string, body []byte) error {
	type frame struct {
		keys      map[string]bool // nil for an array
		expectKey bool
	}
	const maxDocDepth = 256
	dec := json.NewDecoder(bytes.NewReader(body))
	var stack []*frame
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("spec: parse %s: %w", where, err)
		}
		top := func() *frame {
			if len(stack) == 0 {
				return nil
			}
			return stack[len(stack)-1]
		}
		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '{', '[':
				if len(stack) >= maxDocDepth {
					return fmt.Errorf("spec: %s nests deeper than %d levels", where, maxDocDepth)
				}
				next := &frame{}
				if delim == '{' {
					next.keys, next.expectKey = map[string]bool{}, true
				}
				stack = append(stack, next)
				continue
			default:
				stack = stack[:len(stack)-1]
			}
		} else if f := top(); f != nil && f.keys != nil && f.expectKey {
			key, _ := tok.(string)
			if f.keys[key] {
				return fmt.Errorf("spec: %s names the key %q twice in one object; the second silently replaces the first", where, key)
			}
			f.keys[key] = true
			f.expectKey = false
			continue
		}
		// A completed value: inside an object, the next token is a key again.
		if f := top(); f != nil && f.keys != nil {
			f.expectKey = true
		}
	}
}

// checkDocVersion runs the tolerant version pre-pass on EVERY document, not
// only connector.yaml.
//
// A package is distributed as several files and they travel together, so a
// version guard on one of them guards nothing: a decoder ignores fields it
// does not know, which means an ops file written by a newer iterion loads as
// this version with whatever it added silently absent. The failure would not
// be a parse error — it would be an operation that runs and does something
// other than what its author described. A queue-schema rollout cannot cover
// this either: a connector package is distributed independently of the engine.
func checkDocVersion(where string, body []byte) error {
	var probe versionProbe
	if err := yaml.Unmarshal(body, &probe); err != nil {
		return fmt.Errorf("spec: %s is not valid YAML: %w", where, err)
	}
	if probe.SchemaVersion > SchemaVersion {
		return fmt.Errorf("spec: %s declares schema_version %d, newer than supported %d (upgrade iterion)", where, probe.SchemaVersion, SchemaVersion)
	}
	return nil
}

// checkDocConnector refuses a document that belongs to another package.
//
// Each file carries the connector it is part of, and nothing read it. A
// mis-copied ops file would contribute its operations to the wrong package —
// caught downstream by the id-prefix check, but reported as a dozen malformed
// operations rather than as the one fact that explains them.
func checkDocConnector(where, got, want string) error {
	if got == "" || got == want {
		return nil
	}
	return fmt.Errorf("spec: %s says it belongs to connector %q, but this package is %q", where, got, want)
}

// Size reports a package's on-disk byte total, split by part. The split is
// what the packaging decision needs: schemas and ops grow very differently
// with an API's size, and only one of them can be curated down.
type Size struct {
	Connector int64
	Schemas   int64
	Ops       int64
	Files     int
}

// Total is the whole package's byte count.
func (s Size) Total() int64 { return s.Connector + s.Schemas + s.Ops }

// Measure walks a written package directory and totals it.
func Measure(dir string) (Size, error) {
	var out Size
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out.Files++
		switch {
		case filepath.Base(path) == ConnectorFile:
			out.Connector += info.Size()
		case filepath.Base(path) == SchemasFile || filepath.Base(path) == ResponsesFile:
			out.Schemas += info.Size()
		default:
			out.Ops += info.Size()
		}
		return nil
	})
	return out, err
}
