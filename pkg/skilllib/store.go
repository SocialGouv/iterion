package skilllib

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/store"
)

// SkillsDirName is the leaf directory both the global store
// (<GlobalIterionDataDir>/skills) and the per-project override
// (<projectStoreDir>/skills) live under.
const SkillsDirName = "skills"

// Scope constants for a library skill's origin layer.
const (
	ScopeGlobal  = "global"
	ScopeProject = "project"
)

// LibrarySkill is one skill in the library. Name is the canonical on-disk
// identifier (the directory or file basename) — resolution, removal and DSL
// references all key on it; a frontmatter `name:` is NOT allowed to override it
// so List/Resolve stay consistent. Description comes from frontmatter. Body is
// populated only by Get (List leaves it empty for brevity).
type LibrarySkill struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Path        string `json:"-"`     // absolute on-disk path to the SKILL.md / <name>.md
	Scope       string `json:"scope"` // "global" | "project"
	Body        string `json:"body,omitempty"`
}

// Store is the layered skill library: a required global directory plus an
// optional per-project override that shadows the global by name. Modeled on
// pkg/secrets.LayeredGenericSecretStore, minus sealing — skills are not
// secrets.
type Store struct {
	globalDir  string
	projectDir string // "" when there is no distinct project override
}

// NewStore builds a store over an explicit global dir and an optional project
// dir. projectDir "" (or equal to globalDir) disables the project layer.
func NewStore(globalDir, projectDir string) *Store {
	if projectDir != "" && absDir(projectDir) == absDir(globalDir) {
		projectDir = ""
	}
	return &Store{globalDir: globalDir, projectDir: projectDir}
}

// LocalStoreForProject builds the standard local library: global
// <GlobalIterionDataDir>/skills plus a per-project <projectStoreDir>/skills
// override when projectStoreDir is a distinct, non-empty directory (the run's
// resolved `.iterion` store dir). Pass "" for a global-only store.
func LocalStoreForProject(projectStoreDir string) *Store {
	globalDir := filepath.Join(store.GlobalIterionDataDir(), SkillsDirName)
	projectDir := ""
	if projectStoreDir != "" {
		projectDir = filepath.Join(projectStoreDir, SkillsDirName)
	}
	return NewStore(globalDir, projectDir)
}

// HasProject reports whether a project override layer is configured.
func (s *Store) HasProject() bool { return s.projectDir != "" }

// List returns every skill across both layers, project shadowing global by
// name, sorted by name. Descriptions are parsed from frontmatter; bodies are
// omitted (use Get).
func (s *Store) List() ([]LibrarySkill, error) {
	byName := map[string]LibrarySkill{}
	g, err := listDir(s.globalDir, ScopeGlobal)
	if err != nil {
		return nil, err
	}
	for _, sk := range g {
		byName[sk.Name] = sk
	}
	if s.projectDir != "" {
		p, err := listDir(s.projectDir, ScopeProject)
		if err != nil {
			return nil, err
		}
		for _, sk := range p {
			byName[sk.Name] = sk // project overrides global
		}
	}
	out := make([]LibrarySkill, 0, len(byName))
	for _, sk := range byName {
		out = append(out, sk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get returns one skill (resolved project-then-global), including its full
// body. Returns an error when the name resolves to no file.
func (s *Store) Get(name string) (LibrarySkill, error) {
	if err := ValidName(name); err != nil {
		return LibrarySkill{}, err
	}
	path, scope, ok := s.resolve(name)
	if !ok {
		return LibrarySkill{}, fmt.Errorf("skilllib: skill %q not found", name)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return LibrarySkill{}, fmt.Errorf("skilllib: read %s: %w", path, err)
	}
	_, desc := ScanFrontmatter(strings.NewReader(string(data)))
	return LibrarySkill{Name: name, Description: desc, Path: path, Scope: scope, Body: string(data)}, nil
}

// Put writes (creates or overwrites) a skill body at the given scope. When a
// flat <name>.md already exists in that scope it is rewritten in place;
// otherwise the canonical directory form <name>/SKILL.md is written. Durable
// (atomic write + fsync).
func (s *Store) Put(name, body, scope string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	dir, err := s.dirForScope(scope)
	if err != nil {
		return err
	}
	// Rewrite an existing flat file in place; otherwise use directory form.
	dest := filepath.Join(dir, name, "SKILL.md")
	if flat := filepath.Join(dir, name+".md"); fileExists(flat) {
		dest = flat
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("skilllib: mkdir %s: %w", filepath.Dir(dest), err)
	}
	if err := store.WriteFileAtomic(dest, []byte(body), 0o644); err != nil {
		return fmt.Errorf("skilllib: write %s: %w", dest, err)
	}
	return nil
}

// Remove deletes a skill at the given scope (whichever on-disk form exists).
// Returns an error when the skill does not exist in that scope.
func (s *Store) Remove(name, scope string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	dir, err := s.dirForScope(scope)
	if err != nil {
		return err
	}
	if p, ok := skillFilePath(dir, name); ok {
		// Directory form: remove the whole <name>/ dir (it may carry
		// auxiliary files). Flat form: remove just the file.
		if filepath.Base(p) == "SKILL.md" && filepath.Base(filepath.Dir(p)) == name {
			return os.RemoveAll(filepath.Dir(p))
		}
		return os.Remove(p)
	}
	return fmt.Errorf("skilllib: skill %q not found in %s scope", name, scopeLabel(scope))
}

// Resolve returns the on-disk path for name (project override preferred over
// global) and whether it exists. Used by the runtime mirror.
func (s *Store) Resolve(name string) (string, bool) {
	path, _, ok := s.resolve(name)
	return path, ok
}

func (s *Store) resolve(name string) (path, scope string, ok bool) {
	if s.projectDir != "" {
		if p, found := skillFilePath(s.projectDir, name); found {
			return p, ScopeProject, true
		}
	}
	if p, found := skillFilePath(s.globalDir, name); found {
		return p, ScopeGlobal, true
	}
	return "", "", false
}

func (s *Store) dirForScope(scope string) (string, error) {
	switch scope {
	case ScopeGlobal, "":
		return s.globalDir, nil
	case ScopeProject:
		if s.projectDir == "" {
			return "", fmt.Errorf("skilllib: no project scope available (not in a project store)")
		}
		return s.projectDir, nil
	default:
		return "", fmt.Errorf("skilllib: unknown scope %q (want %q or %q)", scope, ScopeGlobal, ScopeProject)
	}
}

// ValidName rejects names that would escape the store dir or collide with the
// on-disk layout. A library skill name is a single path segment.
func ValidName(name string) error {
	if name == "" {
		return fmt.Errorf("skilllib: skill name must not be empty")
	}
	if name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("skilllib: invalid skill name %q (no path separators)", name)
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("skilllib: invalid skill name %q (must not start with a dot)", name)
	}
	return nil
}

// listDir enumerates the skills directly under dir (directory form
// <name>/SKILL.md and flat <name>.md), tagging each with scope. A missing dir
// yields no skills (not an error).
func listDir(dir, scope string) ([]LibrarySkill, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("skilllib: read %s: %w", dir, err)
	}
	var out []LibrarySkill
	for _, e := range entries {
		raw := e.Name()
		if strings.HasPrefix(raw, ".") {
			continue
		}
		var name, path string
		if e.IsDir() {
			p := filepath.Join(dir, raw, "SKILL.md")
			if !fileExists(p) {
				continue
			}
			name, path = raw, p
		} else {
			if !strings.EqualFold(filepath.Ext(raw), ".md") {
				continue
			}
			name = strings.TrimSuffix(raw, filepath.Ext(raw))
			path = filepath.Join(dir, raw)
		}
		sk := LibrarySkill{Name: name, Path: path, Scope: scope}
		if f, err := os.Open(path); err == nil {
			_, sk.Description = ScanFrontmatter(f)
			_ = f.Close()
		}
		out = append(out, sk)
	}
	return out, nil
}

// skillFilePath returns the SKILL.md path for name in dir, preferring the
// directory form <dir>/<name>/SKILL.md then the flat <dir>/<name>.md.
func skillFilePath(dir, name string) (string, bool) {
	if p := filepath.Join(dir, name, "SKILL.md"); fileExists(p) {
		return p, true
	}
	if p := filepath.Join(dir, name+".md"); fileExists(p) {
		return p, true
	}
	return "", false
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

func absDir(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

func scopeLabel(scope string) string {
	if scope == "" {
		return ScopeGlobal
	}
	return scope
}
