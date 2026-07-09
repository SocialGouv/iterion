package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/plugin"
	"github.com/SocialGouv/iterion/pkg/skilllib"
	"github.com/SocialGouv/iterion/pkg/store"
)

// SkillOptions carries the shared inputs for the `iterion skill` subcommands.
// StoreDir is the per-project store dir (the `.iterion` the run store uses);
// empty resolves it from the working directory. Project selects the per-project
// layer (<store>/.iterion/skills) over the machine-global one
// (~/.iterion/skills) for add/rm.
type SkillOptions struct {
	Name     string
	From     string // file to read the skill body from (add); empty = stdin
	Project  bool
	StoreDir string
	Dir      string // export destination dir
}

func (o SkillOptions) scope() string {
	if o.Project {
		return skilllib.ScopeProject
	}
	return skilllib.ScopeGlobal
}

// buildSkillStore resolves the layered skill library for the CLI: global
// ~/.iterion/skills plus the per-project <store>/.iterion/skills override.
func buildSkillStore(opts SkillOptions) *skilllib.Store {
	projectDir := store.ResolveStoreDir(cwd(), opts.StoreDir)
	return skilllib.LocalStoreForProject(projectDir)
}

// RunSkillList prints every library skill (both layers) with scope + description.
func RunSkillList(p *Printer, opts SkillOptions) error {
	skills, err := buildSkillStore(opts).List()
	if err != nil {
		return err
	}
	if p.Format == OutputJSON {
		p.JSON(skills)
		return nil
	}
	if len(skills) == 0 {
		p.Line("No skills in the library. Add one with `iterion skill add <name> --from <file>`.")
		return nil
	}
	rows := make([][]string, 0, len(skills))
	for _, s := range skills {
		rows = append(rows, []string{s.Name, s.Scope, s.Description})
	}
	p.Table([]string{"NAME", "SCOPE", "DESCRIPTION"}, rows)
	return nil
}

// RunSkillShow prints the resolved path + full body of one skill.
func RunSkillShow(p *Printer, opts SkillOptions) error {
	sk, err := buildSkillStore(opts).Get(opts.Name)
	if err != nil {
		return err
	}
	if p.Format == OutputJSON {
		p.JSON(sk)
		return nil
	}
	p.KV("Name", sk.Name)
	p.KV("Scope", sk.Scope)
	p.KV("Path", sk.Path)
	if sk.Description != "" {
		p.KV("Description", sk.Description)
	}
	p.Blank()
	p.Line("%s", sk.Body)
	return nil
}

// RunSkillAdd creates or overwrites a skill from a file (--from) or stdin.
func RunSkillAdd(p *Printer, opts SkillOptions) error {
	name := strings.TrimSpace(opts.Name)
	if err := skilllib.ValidName(name); err != nil {
		return err
	}
	body, err := readSkillBody(opts.From)
	if err != nil {
		return err
	}
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("empty skill body")
	}
	st := buildSkillStore(opts)
	scope := opts.scope()
	if scope == skilllib.ScopeProject && !st.HasProject() {
		scope = skilllib.ScopeGlobal
	}
	if err := st.Put(name, body, scope); err != nil {
		return err
	}
	p.Line("Stored skill %q (%s scope)", name, scope)
	return nil
}

// RunSkillRemove deletes a skill at the selected scope.
func RunSkillRemove(p *Printer, opts SkillOptions) error {
	name := strings.TrimSpace(opts.Name)
	if err := skilllib.ValidName(name); err != nil {
		return err
	}
	st := buildSkillStore(opts)
	scope := opts.scope()
	if scope == skilllib.ScopeProject && !st.HasProject() {
		scope = skilllib.ScopeGlobal
	}
	if err := st.Remove(name, scope); err != nil {
		return err
	}
	p.Line("Removed skill %q (%s scope)", name, scope)
	return nil
}

// RunSkillExport copies a skill's file(s) out to a destination directory.
func RunSkillExport(p *Printer, opts SkillOptions) error {
	sk, err := buildSkillStore(opts).Get(opts.Name)
	if err != nil {
		return err
	}
	dstDir := opts.Dir
	if dstDir == "" {
		dstDir = cwd()
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(dstDir, sk.Name+".md")
	if err := os.WriteFile(dst, []byte(sk.Body), 0o644); err != nil {
		return err
	}
	p.Line("Exported skill %q → %s", sk.Name, dst)
	return nil
}

// RunSkillImport installs a public skill library (a bare skills/ git repo or a
// local dir) via the plugin skills-only-manifest path — the third-party-pack
// half of the hybride model. The installed pack is enable/disable-able via
// `iterion plugin`; enable it to make its skills mirror into runs.
func RunSkillImport(p *Printer, opts SkillOptions) error {
	src := strings.TrimSpace(opts.Name)
	if src == "" {
		return fmt.Errorf("skill import requires a git URL or local directory")
	}
	name, err := plugin.Install(context.Background(), src)
	if err != nil {
		return err
	}
	p.Line("Imported skill pack %q (disabled by default). Enable with `iterion plugin enable %s`.", name, name)
	return nil
}

// readSkillBody reads a skill body from a file path, or stdin when path is "".
func readSkillBody(path string) (string, error) {
	if path == "" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return string(data), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return string(data), nil
}
