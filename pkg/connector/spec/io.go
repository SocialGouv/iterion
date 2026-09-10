package spec

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v2"
)

// File names inside a connector package directory.
const (
	ConnectorFile = "connector.yaml"
	SchemasFile   = "schemas.yaml"
	OpsDir        = "ops"
)

// Write materialises a package as a directory. Every file is deterministic —
// sorted maps, sorted operations — so regenerating an unchanged description
// produces byte-identical output and a diff shows only real change. That is
// what makes a generated package reviewable in a PR rather than a blob.
func Write(dir string, p *Package) error {
	if err := os.MkdirAll(filepath.Join(dir, OpsDir), 0o755); err != nil {
		return fmt.Errorf("spec: create %s: %w", dir, err)
	}
	if err := writeYAML(filepath.Join(dir, ConnectorFile), p.Connector); err != nil {
		return err
	}
	if len(p.Schemas) > 0 {
		sf := schemasDoc{SchemaVersion: SchemaVersion, Connector: p.Connector.ID, Schemas: p.Schemas}
		if err := writeYAML(filepath.Join(dir, SchemasFile), sf); err != nil {
			return err
		}
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

func writeYAML(path string, v any) error {
	body, err := yaml.Marshal(v)
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

// Load reads a connector package from a directory and runs the COMPLETE
// check. It is the launch/publish path: what it returns is usable.
func Load(dir string) (*Package, error) {
	p, err := read(dir)
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
func LoadGenerated(dir string) (*Package, error) {
	p, err := read(dir)
	if err != nil {
		return nil, err
	}
	if err := p.ValidateGenerated(); err != nil {
		return nil, err
	}
	return p, nil
}

func read(dir string) (*Package, error) {
	body, err := os.ReadFile(filepath.Join(dir, ConnectorFile))
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
	if err := yaml.UnmarshalStrict(body, &c); err != nil {
		return nil, fmt.Errorf("spec: parse %s: %w", ConnectorFile, err)
	}
	p := &Package{Connector: c, Schemas: map[string]Schema{}}

	if sb, err := os.ReadFile(filepath.Join(dir, SchemasFile)); err == nil {
		var sf schemasDoc
		if err := yaml.UnmarshalStrict(sb, &sf); err != nil {
			return nil, fmt.Errorf("spec: parse %s: %w", SchemasFile, err)
		}
		p.Schemas = sf.Schemas
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("spec: read %s: %w", SchemasFile, err)
	}

	opsDir := filepath.Join(dir, OpsDir)
	entries, err := os.ReadDir(opsDir)
	if err != nil && !os.IsNotExist(err) {
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
		ob, err := os.ReadFile(filepath.Join(opsDir, name))
		if err != nil {
			return nil, fmt.Errorf("spec: read %s/%s: %w", OpsDir, name, err)
		}
		var f OpsFile
		if err := yaml.UnmarshalStrict(ob, &f); err != nil {
			return nil, fmt.Errorf("spec: parse %s/%s: %w", OpsDir, name, err)
		}
		p.Ops = append(p.Ops, f)
	}
	return p, nil
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
		case filepath.Base(path) == SchemasFile:
			out.Schemas += info.Size()
		default:
			out.Ops += info.Size()
		}
		return nil
	})
	return out, err
}
