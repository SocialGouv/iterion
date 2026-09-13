package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/botinstall"
	"github.com/SocialGouv/iterion/pkg/botlock"
	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/server/projects"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/subbotsource"
)

type dependencyTransactionFixture struct {
	server                             *Server
	req                                assistantDependencyBotsUpdateRequest
	root, source, cache, catalog, head string
	lock, index                        []byte
	cacheHash                          string
}

func newDependencyTransactionFixture(t *testing.T, installed, catalog bool) dependencyTransactionFixture {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "consumer")
	source := filepath.Join(base, "source")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAssistantDependencyBundle(t, source, "0.1.0", "workflow shared {}\n")
	manifest := filepath.Join(source, "manifest.yaml")
	body, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeGitTestFile(t, manifest, string(body)+"exports:\n  workflows:\n    - id: shared\n      path: main.bot\n", 0o644)
	catalogTemplate := "Catalog\n<!-- ITERION:CATALOG:GENERATED:BEGIN -->\nstale\n<!-- ITERION:CATALOG:GENERATED:END -->\n"
	if catalog {
		if err := os.Mkdir(filepath.Join(source, "skills"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeGitTestFile(t, filepath.Join(source, "iterion-bot-catalog-static.md"), catalogTemplate, 0o644)
		writeGitTestFile(t, filepath.Join(source, "skills", "iterion-bot-catalog.md"), "old pinned catalog\n", 0o644)
	}
	initAuthoringGit(t, source)
	oldRef := strings.TrimSpace(gitTestRead(t, source, "rev-parse", "HEAD"))
	oldHash, err := bundle.ContentHashDir(source)
	if err != nil {
		t.Fatal(err)
	}
	consumer := filepath.Join(root, "bots", "consumer")
	if err := os.MkdirAll(consumer, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGitTestFile(t, filepath.Join(consumer, "main.bot"), "workflow consumer {}\n", 0o644)
	writeGitTestFile(t, filepath.Join(consumer, "manifest.yaml"), "name: consumer\nschema_version: 1\ndependencies:\n  workflows:\n    - name: shared-planner\n", 0o644)
	catalogPath := ""
	if catalog {
		if err := os.Mkdir(filepath.Join(consumer, "skills"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeGitTestFile(t, filepath.Join(consumer, "iterion-bot-catalog-static.md"), catalogTemplate, 0o644)
		catalogPath = filepath.Join(consumer, "skills", "iterion-bot-catalog.md")
		writeGitTestFile(t, catalogPath, "stale operator catalog\n", 0o644)
	}
	writeGitTestFile(t, filepath.Join(root, ".gitignore"), ".botz/\n", 0o644)
	lock := fmt.Sprintf("# Preserve exact source formatting on rejection.\nversion: 1\ndependencies:\n  shared-planner:\n    source: ../source\n    ref: %s\n    bundle_sha256: %s\n", oldRef, oldHash)
	writeGitTestFile(t, filepath.Join(root, "bots.lock"), lock, 0o640)
	initAuthoringGit(t, root)
	cache := filepath.Join(root, ".botz", "shared-planner")
	if installed {
		if err := copyDependencyLocalizationTree(source, cache); err != nil {
			t.Fatal(err)
		}
	}
	writeGitTestFile(t, filepath.Join(source, "main.bot"), "workflow refreshed {}\n", 0o644)
	if catalog {
		writeGitTestFile(t, filepath.Join(source, "skills", "iterion-bot-catalog.md"), "new pinned catalog\n", 0o644)
	}
	gitTestRead(t, source, "add", ".")
	gitTestRead(t, source, "commit", "-qm", "refresh source")
	index, err := os.ReadFile(filepath.Join(root, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	return dependencyTransactionFixture{server: &Server{cfg: Config{WorkDir: root}}, req: assistantDependencyBotsUpdateRequest{Name: "shared-planner", Ref: strings.TrimSpace(gitTestRead(t, source, "rev-parse", "HEAD")), Message: "chore(bots): refresh shared planner"}, root: root, source: source, cache: cache, catalog: catalogPath, head: strings.TrimSpace(gitTestRead(t, root, "rev-parse", "HEAD")), lock: []byte(lock), index: index, cacheHash: oldHash}
}

func (f dependencyTransactionFixture) assertRestored(t *testing.T, installed bool) {
	t.Helper()
	for path, want := range map[string][]byte{filepath.Join(f.root, "bots.lock"): f.lock, filepath.Join(f.root, ".git", "index"): f.index} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s changed after rejected update: %v", path, err)
		}
	}
	info, err := os.Stat(filepath.Join(f.root, "bots.lock"))
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("lock mode: %v %v", info, err)
	}
	if got := strings.TrimSpace(gitTestRead(t, f.root, "rev-parse", "HEAD")); got != f.head {
		t.Fatalf("HEAD changed to %s", got)
	}
	if installed {
		if got, err := bundle.ContentHashDir(f.cache); err != nil || got != f.cacheHash {
			t.Fatalf("restored cache hash %s: %v", got, err)
		}
		f.assertResolves(t)
	} else if _, err := os.Lstat(f.cache); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("originally absent cache exists: %v", err)
	}
	f.assertCatalog(t)
}

func (f dependencyTransactionFixture) assertResolves(t *testing.T) {
	t.Helper()
	_, err := subbotsource.NewResolver(subbotsource.ResolverOptions{WorkDir: f.root}).Resolve(t.Context(), filepath.Join(f.root, "bots", "consumer", "main.bot"), "bot://shared-planner/shared")
	if err != nil {
		t.Fatalf("real dependency resolver: %v", err)
	}
}

func (f dependencyTransactionFixture) assertCatalog(t *testing.T) {
	t.Helper()
	if f.catalog == "" {
		return
	}
	got, err := os.ReadFile(f.catalog)
	if err != nil || string(got) != "stale operator catalog\n" {
		t.Fatalf("unrelated catalog changed: %q %v", got, err)
	}
}

func (f dependencyTransactionFixture) assertNoRecovery(t *testing.T) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(f.root, ".git", "iterion-dependency-*"))
	if err != nil || len(paths) != 0 {
		t.Fatalf("recovery leak %v: %v", paths, err)
	}
}

func prepareDependencyTestTransaction(t *testing.T, f dependencyTransactionFixture) *assistantDependencyTransaction {
	t.Helper()
	tx, err := prepareAssistantDependencyTransaction(t.Context(), f.root, f.root, "bots.lock", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.prepare(t.Context(), f.req); err != nil {
		t.Fatal(err)
	}
	return tx
}

func commitDependencyTestTransaction(ctx context.Context, tx *assistantDependencyTransaction) (string, error) {
	hash, err := commitAttestedAuthoringFilesWithDependency(ctx, tx.root, []string{"bots.lock"}, map[string]string{"bots.lock": contentSHA256(string(tx.candidateLock))}, "chore(bots): update dependency", tx)
	return hash, tx.discard(err)
}

func TestAssistantDependencyTransactionHookRollbackAndRetry(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(fmt.Sprint(installed), func(t *testing.T) {
			f := newDependencyTransactionFixture(t, installed, true)
			gitTestHook(t, f.root, "pre-commit", "exit 73")
			rec := assistantDependencyCall(t, f.server, f.req)
			if rec.Code == 200 {
				t.Fatal("rejecting hook accepted")
			}
			f.assertRestored(t, installed)
			f.assertNoRecovery(t)
			if err := os.Remove(filepath.Join(f.root, ".git", "hooks", "pre-commit")); err != nil {
				t.Fatal(err)
			}
			rec = assistantDependencyCall(t, f.server, f.req)
			if rec.Code != 200 {
				t.Fatalf("retry %d %s", rec.Code, rec.Body.String())
			}
			f.assertResolves(t)
			f.assertCatalog(t)
			f.assertNoRecovery(t)
			if got := gitTestRead(t, f.root, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD"); strings.TrimSpace(got) != "bots.lock" {
				t.Fatalf("published unexpected paths %s", got)
			}
			got, err := os.ReadFile(filepath.Join(f.cache, "skills", "iterion-bot-catalog.md"))
			if err != nil || string(got) != "new pinned catalog\n" {
				t.Fatalf("candidate catalog %q %v", got, err)
			}
		})
	}
}

func TestAssistantDependencyTransactionPreparationFailures(t *testing.T) {
	for _, failure := range []string{"invalid-ref", "materialization", "discovery", "cache-symlink", "parent-symlink"} {
		t.Run(failure, func(t *testing.T) {
			f := newDependencyTransactionFixture(t, true, true)
			var err error
			switch failure {
			case "invalid-ref":
				f.req.Ref = strings.Repeat("a", 40)
				_, err = assistantDependencyBotsUpdate(t.Context(), f.root, f.req)
			case "materialization":
				tx, prepErr := prepareAssistantDependencyTransaction(t.Context(), f.root, f.root, "bots.lock", nil)
				if prepErr != nil {
					t.Fatal(prepErr)
				}
				// Install refuses an existing private target, leaving live state untouched.
				if e := os.Mkdir(filepath.Join(tx.stageParent, f.req.Name), 0o700); e != nil {
					t.Fatal(e)
				}
				_, err = tx.prepare(t.Context(), f.req)
				err = tx.discard(err)
			case "discovery":
				_, err = assistantDependencyBotsUpdateInPaths(t.Context(), f.root, f.req, []string{f.root})
			case "cache-symlink", "parent-symlink":
				path := f.cache
				if failure == "parent-symlink" {
					path = filepath.Dir(f.cache)
				}
				saved := path + "-saved"
				if e := os.Rename(path, saved); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink(saved, path); e != nil {
					t.Fatal(e)
				}
				_, err = assistantDependencyBotsUpdate(t.Context(), f.root, f.req)
				if e := os.Remove(path); e != nil {
					t.Fatal(e)
				}
				if e := os.Rename(saved, path); e != nil {
					t.Fatal(e)
				}
			}
			if err == nil {
				t.Fatal("expected preparation rejection")
			}
			f.assertRestored(t, true)
			f.assertNoRecovery(t)
		})
	}
}

func TestAssistantDependencyTransactionPartialMovesAndCancellation(t *testing.T) {
	for _, installed := range []bool{false, true} {
		for _, failure := range []string{"rename", "EXDEV", "cancel"} {
			t.Run(fmt.Sprintf("%t/%s", installed, failure), func(t *testing.T) {
				f := newDependencyTransactionFixture(t, installed, false)
				tx := prepareDependencyTestTransaction(t, f)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				move := tx.rename
				tx.rename = func(from, to string) error {
					if from == tx.stagePath && to == tx.cachePath {
						switch failure {
						case "rename":
							return errors.New("injected candidate move failure")
						case "EXDEV":
							return syscall.EXDEV
						}
					}
					err := move(from, to)
					if from == tx.stagePath && failure == "cancel" {
						cancel()
					}
					return err
				}
				hash, err := commitDependencyTestTransaction(ctx, tx)
				if err == nil || hash != "" {
					t.Fatalf("failure not reported: %s %v", hash, err)
				}
				f.assertRestored(t, installed)
				f.assertNoRecovery(t)
			})
		}
	}
}

func TestAssistantDependencyTransactionPreservesConcurrentWork(t *testing.T) {
	for _, change := range []string{"lock", "cache", "runtime-cache", "staged", "HEAD"} {
		t.Run(change, func(t *testing.T) {
			f := newDependencyTransactionFixture(t, true, false)
			tx := prepareDependencyTestTransaction(t, f)
			path := filepath.Join(f.root, "notes.txt")
			switch change {
			case "lock":
				path = filepath.Join(f.root, "bots.lock")
			case "cache":
				path = filepath.Join(f.cache, "main.bot")
			case "runtime-cache":
				path = filepath.Join(f.cache, "runtime.pyc")
			}
			writeGitTestFile(t, path, "independent edit\n", 0o640)
			if change == "staged" || change == "HEAD" {
				gitTestRead(t, f.root, "add", "--", "notes.txt")
			}
			if change == "HEAD" {
				gitTestRead(t, f.root, "commit", "-qm", "independent")
			}
			expectedHead := gitTestRead(t, f.root, "rev-parse", "HEAD")
			hash, err := commitDependencyTestTransaction(t.Context(), tx)
			if err == nil || hash != "" {
				t.Fatalf("concurrent change accepted: %s %v", hash, err)
			}
			body, readErr := os.ReadFile(path)
			if readErr != nil || string(body) != "independent edit\n" {
				t.Fatalf("lost concurrent edit %q %v", body, readErr)
			}
			if got := gitTestRead(t, f.root, "rev-parse", "HEAD"); got != expectedHead {
				t.Fatal("changed independent HEAD")
			}
			f.assertNoRecovery(t)
		})
	}
}

func TestAssistantDependencyTransactionWithholdsJointRollback(t *testing.T) {
	for _, changed := range []string{"lock", "cache", "runtime-cache"} {
		t.Run(changed, func(t *testing.T) {
			f := newDependencyTransactionFixture(t, true, false)
			path := "bots.lock"
			if changed == "cache" {
				path = ".botz/shared-planner/main.bot"
			}
			if changed == "runtime-cache" {
				path = ".botz/shared-planner/runtime.pyc"
			}
			gitTestHook(t, f.root, "pre-commit", "printf 'independent hook edit\\n' > "+path+"\nexit 73")
			rec := assistantDependencyCall(t, f.server, f.req)
			if rec.Code == 200 || !strings.Contains(rec.Body.String(), "joint restoration withheld") || !strings.Contains(rec.Body.String(), "recovery material retained at") {
				t.Fatalf("missing retained recovery diagnostic: %d %s", rec.Code, rec.Body.String())
			}
			body, err := os.ReadFile(filepath.Join(f.root, path))
			if err != nil || string(body) != "independent hook edit\n" {
				t.Fatalf("hook edit lost %q %v", body, err)
			}
			paths, err := filepath.Glob(filepath.Join(f.root, ".git", "iterion-dependency-*", "original-cache"))
			if err != nil || len(paths) != 1 {
				t.Fatalf("backup lost: %v %v", paths, err)
			}
			hash, err := bundle.ContentHashDir(paths[0])
			if err != nil || hash != f.cacheHash {
				t.Fatalf("original backup invalid %s %v", hash, err)
			}
			if changed != "lock" {
				lock, err := botlock.Load(f.root)
				if err != nil || lock.Dependencies[f.req.Name].Ref != f.req.Ref {
					t.Fatal("restored only one member of an interfered transaction")
				}
			}
		})
	}
}

func TestAssistantDependencyTransactionStagingFailure(t *testing.T) {
	f := newDependencyTransactionFixture(t, true, false)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrappers := t.TempDir()
	t.Setenv("AUTHORING_REAL_GIT", realGit)
	writeGitTestFile(t, filepath.Join(wrappers, "git"), `#!/bin/sh
for arg do
 if [ "$arg" = add ]; then exit 73; fi
done
exec "$AUTHORING_REAL_GIT" "$@"
`, 0o755)
	t.Setenv("PATH", wrappers+string(os.PathListSeparator)+os.Getenv("PATH"))
	rec := assistantDependencyCall(t, f.server, f.req)
	if rec.Code == 200 {
		t.Fatal("injected staging failure accepted")
	}
	f.assertRestored(t, true)
	f.assertNoRecovery(t)
}

func installOriginalDependencyOrigin(t *testing.T, f dependencyTransactionFixture) (string, []byte) {
	t.Helper()
	path := filepath.Join(f.root, ".botz", ".origins", f.req.Name+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte("{\n  \"source\": \"operator-origin\",\n  \"ref\": \"old-pin\"\n}\n")
	if err := os.WriteFile(path, body, 0o640); err != nil {
		t.Fatal(err)
	}
	return path, body
}

func TestAssistantDependencyOriginRollbackAndRetry(t *testing.T) {
	for _, cachePresent := range []bool{false, true} {
		for _, originPresent := range []bool{false, true} {
			t.Run(fmt.Sprintf("cache=%t/origin=%t", cachePresent, originPresent), func(t *testing.T) {
				f := newDependencyTransactionFixture(t, cachePresent, true)
				originPath := filepath.Join(f.root, ".botz", ".origins", f.req.Name+".json")
				var original []byte
				if originPresent {
					originPath, original = installOriginalDependencyOrigin(t, f)
				}
				parentBefore, parentErr := os.Lstat(filepath.Dir(originPath))
				gitTestHook(t, f.root, "commit-msg", "exit 73")
				rec := assistantDependencyCall(t, f.server, f.req)
				if rec.Code == 200 {
					t.Fatal("rejecting hook accepted")
				}
				f.assertRestored(t, cachePresent)
				f.assertNoRecovery(t)
				if originPresent {
					body, err := os.ReadFile(originPath)
					if err != nil || !bytes.Equal(body, original) {
						t.Fatalf("origin not restored: %q %v", body, err)
					}
					info, err := os.Stat(originPath)
					if err != nil || info.Mode().Perm() != 0o640 {
						t.Fatalf("origin mode %v %v", info, err)
					}
				} else if _, err := os.Lstat(originPath); !os.IsNotExist(err) {
					t.Fatalf("origin should be absent: %v", err)
				}
				after, err := os.Lstat(filepath.Dir(originPath))
				if os.IsNotExist(parentErr) {
					if !os.IsNotExist(err) {
						t.Fatalf("created origin parent leaked: %v", err)
					}
				} else if err != nil || !os.SameFile(parentBefore, after) {
					t.Fatal("pre-existing origin parent replaced")
				}
				if err := os.Remove(filepath.Join(f.root, ".git", "hooks", "commit-msg")); err != nil {
					t.Fatal(err)
				}
				rec = assistantDependencyCall(t, f.server, f.req)
				if rec.Code != 200 {
					t.Fatalf("retry %d %s", rec.Code, rec.Body.String())
				}
				origin, err := botinstall.ReadOrigin(f.cache)
				if err != nil {
					t.Fatal(err)
				}
				if origin.Source != f.source || origin.Ref != f.req.Ref || origin.SourcePath != "" || origin.InstalledAt.IsZero() {
					t.Fatalf("non-durable candidate provenance: %+v", origin)
				}
				f.assertResolves(t)
				f.assertCatalog(t)
				f.assertNoRecovery(t)
			})
		}
	}
}

func TestAssistantDependencyOriginMoveFailures(t *testing.T) {
	for _, present := range []bool{false, true} {
		for _, step := range []string{"backup", "install"} {
			t.Run(fmt.Sprintf("%t/%s", present, step), func(t *testing.T) {
				f := newDependencyTransactionFixture(t, true, false)
				path := filepath.Join(f.root, ".botz", ".origins", f.req.Name+".json")
				var original []byte
				if present {
					path, original = installOriginalDependencyOrigin(t, f)
				}
				tx := prepareDependencyTestTransaction(t, f)
				move := tx.rename
				tx.rename = func(from, to string) error {
					if (step == "backup" && to == tx.backupOrigin) || (step == "install" && from == tx.stagedOrigin) {
						return errors.New("injected origin move failure")
					}
					return move(from, to)
				}
				// There is no backup step for an absent original; fail installation instead.
				if !present && step == "backup" {
					gitTestHook(t, f.root, "pre-commit", "exit 73")
				}
				_, err := commitDependencyTestTransaction(t.Context(), tx)
				if err == nil {
					t.Fatal("move failure accepted")
				}
				f.assertRestored(t, true)
				f.assertNoRecovery(t)
				body, err := os.ReadFile(path)
				if present {
					if err != nil || !bytes.Equal(body, original) {
						t.Fatalf("original origin lost %q %v", body, err)
					}
				} else if !os.IsNotExist(err) {
					t.Fatalf("origin should be absent: %v", err)
				}
			})
		}
	}
}

func TestAssistantDependencyOriginInterference(t *testing.T) {
	for _, when := range []string{"preparation", "hook"} {
		for _, change := range []string{"edit", "replace", "symlink", "parent-symlink", "parent-mode"} {
			t.Run(when+"/"+change, func(t *testing.T) {
				f := newDependencyTransactionFixture(t, true, false)
				originPath, _ := installOriginalDependencyOrigin(t, f)
				script := "printf 'independent origin\\n' > .botz/.origins/shared-planner.json"
				switch change {
				case "replace":
					script = "rm .botz/.origins/shared-planner.json\nprintf 'independent origin\\n' > .botz/.origins/shared-planner.json"
				case "symlink":
					script = "mv .botz/.origins/shared-planner.json .botz/.origins/kept.json\nln -s kept.json .botz/.origins/shared-planner.json"
				case "parent-symlink":
					script = "mv .botz/.origins .botz/origins-kept\nln -s origins-kept .botz/.origins"
				case "parent-mode":
					script = "chmod 0755 .botz/.origins"
				}
				var err error
				if when == "hook" {
					gitTestHook(t, f.root, "pre-commit", script+"\nexit 73")
					_, err = assistantDependencyBotsUpdate(t.Context(), f.root, f.req)
					if err == nil || !strings.Contains(err.Error(), "joint restoration withheld") {
						t.Fatalf("origin interference restored other members: %v", err)
					}
					lock, loadErr := botlock.Load(f.root)
					if loadErr != nil || lock.Dependencies[f.req.Name].Ref != f.req.Ref {
						t.Fatalf("candidate lock was wrongly restored: %v", loadErr)
					}
					paths, _ := filepath.Glob(filepath.Join(f.root, ".git", "iterion-dependency-*", "original-origin.json"))
					if len(paths) != 1 {
						t.Fatalf("original origin backup lost: %v", paths)
					}
				} else {
					tx := prepareDependencyTestTransaction(t, f)
					cmd := exec.CommandContext(t.Context(), "sh", "-c", script)
					cmd.Dir = f.root
					if output, e := cmd.CombinedOutput(); e != nil {
						t.Fatalf("interference fixture: %s %v", output, e)
					}
					_, err = commitDependencyTestTransaction(t.Context(), tx)
					if err == nil {
						t.Fatal("origin changed during preparation was accepted")
					}
				}
				switch change {
				case "edit", "replace":
					body, e := os.ReadFile(originPath)
					if e != nil || string(body) != "independent origin\n" {
						t.Fatalf("origin edit lost: %q %v", body, e)
					}
				case "symlink":
					info, e := os.Lstat(originPath)
					if e != nil || info.Mode()&os.ModeSymlink == 0 {
						t.Fatal("origin symlink removed")
					}
				case "parent-symlink":
					info, e := os.Lstat(filepath.Dir(originPath))
					if e != nil || info.Mode()&os.ModeSymlink == 0 {
						t.Fatal("parent symlink removed")
					}
				case "parent-mode":
					info, e := os.Stat(filepath.Dir(originPath))
					if e != nil || info.Mode().Perm() != 0o755 {
						t.Fatal("parent permissions overwritten")
					}
				}
			})
		}
	}
}

func TestAssistantDependencyOriginRejectsUnsupportedObjects(t *testing.T) {
	for _, kind := range []string{"directory", "symlink", "parent-symlink"} {
		t.Run(kind, func(t *testing.T) {
			f := newDependencyTransactionFixture(t, true, false)
			path := filepath.Join(f.root, ".botz", ".origins")
			if kind == "parent-symlink" {
				if err := os.Symlink(t.TempDir(), path); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(path, f.req.Name+".json")
				if kind == "directory" {
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Symlink(filepath.Join(f.root, "bots.lock"), path); err != nil {
						t.Fatal(err)
					}
				}
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			rec := assistantDependencyCall(t, f.server, f.req)
			if rec.Code == 200 {
				t.Fatal("unsupported origin accepted")
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("unsupported object modified")
			}
			f.assertRestored(t, true)
			f.assertNoRecovery(t)
		})
	}
}

func TestAssistantDependencyOriginSiblingSurvives(t *testing.T) {
	for _, existingParent := range []bool{false, true} {
		t.Run(fmt.Sprint(existingParent), func(t *testing.T) {
			f := newDependencyTransactionFixture(t, true, false)
			sibling := filepath.Join(f.root, ".botz", ".origins", "other.json")
			if existingParent {
				if err := os.Mkdir(filepath.Dir(sibling), 0o700); err != nil {
					t.Fatal(err)
				}
				writeGitTestFile(t, sibling, "other bot\n", 0o600)
			}
			gitTestHook(t, f.root, "pre-commit", "printf 'other bot\\n' > .botz/.origins/other.json\nexit 73")
			_, err := assistantDependencyBotsUpdate(t.Context(), f.root, f.req)
			if err == nil {
				t.Fatal("hook accepted")
			}
			f.assertRestored(t, true)
			body, e := os.ReadFile(sibling)
			if e != nil || string(body) != "other bot\n" {
				t.Fatalf("sibling lost: %q %v", body, e)
			}
			if existingParent {
				f.assertNoRecovery(t)
			} else if !strings.Contains(err.Error(), "owned origin parent cleanup failed") {
				t.Fatalf("nonempty created parent not reported: %v", err)
			}
		})
	}
}

func TestAssistantDependencyTransactionPublishedErrorKeepsAllCandidates(t *testing.T) {
	f := newDependencyTransactionFixture(t, true, false)
	installOriginalDependencyOrigin(t, f)
	gitTestHook(t, f.root, "reference-transaction", `if [ "$1" = committed ]; then
 mv .git/index .git/saved-index
 mkdir .git/index
fi`)
	_, err := assistantDependencyBotsUpdate(t.Context(), f.root, f.req)
	if err == nil || !strings.Contains(err.Error(), "was published") || !strings.Contains(err.Error(), "recovery material retained at") {
		t.Fatalf("publication error misreported: %v", err)
	}
	if head := strings.TrimSpace(gitTestRead(t, f.root, "rev-parse", "HEAD")); head == f.head {
		t.Fatal("no commit published")
	}
	f.assertResolves(t)
	origin, e := botinstall.ReadOrigin(f.cache)
	if e != nil || origin.Ref != f.req.Ref {
		t.Fatalf("published origin rolled back: %+v %v", origin, e)
	}
}

func TestAssistantDependencyTransactionAmbiguousPublicationRetainsMaterial(t *testing.T) {
	f := newDependencyTransactionFixture(t, true, false)
	installOriginalDependencyOrigin(t, f)
	tx := prepareDependencyTestTransaction(t, f)
	lock, err := os.OpenFile(tx.indexPath+".lock", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close(); _ = os.Remove(tx.indexPath + ".lock") }()
	if err := tx.install(t.Context(), tx.head, tx.binding); err != nil {
		t.Fatal(err)
	}
	err = tx.finish(t.Context(), "", &authoringPublicationUncertainError{errors.New("injected lost commit acknowledgement")})
	err = tx.discard(err)
	if err == nil || !strings.Contains(err.Error(), "publication was not rolled back") {
		t.Fatalf("ambiguous publication misreported: %v", err)
	}
	f.assertResolves(t)
	origin, e := botinstall.ReadOrigin(f.cache)
	if e != nil || origin.Ref != f.req.Ref {
		t.Fatalf("candidate origin rolled back: %+v %v", origin, e)
	}
	if _, e := os.Stat(tx.backupOrigin); e != nil {
		t.Fatalf("original origin backup lost: %v", e)
	}
}

func TestAssistantDependencyTransactionIndependentCommitIsPreserved(t *testing.T) {
	f := newDependencyTransactionFixture(t, true, false)
	installOriginalDependencyOrigin(t, f)
	gitTestHook(t, f.root, "pre-commit", `tree=$(git write-tree)
commit=$(printf 'independent commit\n' | git commit-tree "$tree" -p HEAD)
git update-ref HEAD "$commit"`)
	_, err := assistantDependencyBotsUpdate(t.Context(), f.root, f.req)
	if err == nil || !strings.Contains(err.Error(), "joint restoration withheld") {
		t.Fatalf("independent commit not preserved: %v", err)
	}
	if head := strings.TrimSpace(gitTestRead(t, f.root, "rev-parse", "HEAD")); head == f.head {
		t.Fatal("independent HEAD restored")
	}
	f.assertResolves(t)
	origin, e := botinstall.ReadOrigin(f.cache)
	if e != nil || origin.Ref != f.req.Ref {
		t.Fatalf("independently committed provenance rolled back: %+v %v", origin, e)
	}
	if got := gitTestRead(t, f.root, "show", "HEAD:bots.lock"); !strings.Contains(got, f.req.Ref) {
		t.Fatal("independent candidate commit changed")
	}
}

func TestAssistantDependencyTransactionRejectsSupersededPreparation(t *testing.T) {
	f := newDependencyTransactionFixture(t, true, false)
	// Both preparations finish against the same state before either can publish.
	first := prepareDependencyTestTransaction(t, f)
	second := prepareDependencyTestTransaction(t, f)
	hash, err := commitDependencyTestTransaction(t.Context(), first)
	if err != nil || hash == "" {
		t.Fatalf("first update: %s %v", hash, err)
	}
	if _, err := commitDependencyTestTransaction(t.Context(), second); err == nil {
		t.Fatal("superseded preparation published")
	}
	if got := strings.TrimSpace(gitTestRead(t, f.root, "rev-parse", "HEAD")); got != hash {
		t.Fatal("second preparation changed first commit")
	}
	f.assertResolves(t)
	f.assertNoRecovery(t)
}

func TestAssistantDependencyOriginDelegation(t *testing.T) {
	// A fresh test process gives the cached project registry its own config path,
	// independent of earlier tests and of the operator's actual registry.
	if os.Getenv("ITERION_DEPENDENCY_ORIGIN_TEST_CHILD") == "" {
		privateConfig := t.TempDir()
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestAssistantDependencyOriginDelegation$")
		cmd.Env = append(os.Environ(), "ITERION_DEPENDENCY_ORIGIN_TEST_CHILD=1", "XDG_CONFIG_HOME="+privateConfig, "HOME="+privateConfig, "APPDATA="+privateConfig)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated provenance test: %v\n%s", err, output)
		}
		return
	}
	f := newDependencyTransactionFixture(t, true, false)
	cfg, err := projects.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.RecentProjects = []projects.Project{{ID: "consumer", Dir: f.root}, {ID: "source", Dir: f.source}}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	rec := assistantDependencyCall(t, f.server, f.req)
	if rec.Code != 200 {
		t.Fatalf("update %d %s", rec.Code, rec.Body.String())
	}
	run := &store.Run{FilePath: filepath.Join(f.cache, "main.bot")}
	origin, err := inferRunBotOrigin(run)
	if err != nil || origin.Kind != "installed_package" || origin.RepoRoot != f.source || origin.ProjectID != "source" || origin.InstallSource != f.source || origin.InstallRef != f.req.Ref || origin.InstallPath != "" {
		t.Fatalf("local delegation provenance: %+v %v", origin, err)
	}
	// Exercise the existing URL lookup without a network fetch.
	remote := botinstall.Origin{Source: "https://example.invalid/shared.git", Ref: f.req.Ref, SourcePath: "bots/shared-planner"}
	body, err := json.Marshal(remote)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, ".botz", ".origins", f.req.Name+".json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	origin, err = inferRunBotOrigin(run)
	if err != nil || origin.Kind != "installed_package" || origin.RepoURL != remote.Source || origin.Commit != f.req.Ref || origin.InstallPath != remote.SourcePath || origin.RepoRoot != "" {
		t.Fatalf("remote delegation provenance: %+v %v", origin, err)
	}
}

func TestAssistantDependencyOriginParentReplacementBeforeRename(t *testing.T) {
	f := newDependencyTransactionFixture(t, true, false)
	installOriginalDependencyOrigin(t, f)
	tx := prepareDependencyTestTransaction(t, f)
	move := tx.rename
	outside := t.TempDir()
	sentinel := filepath.Join(outside, f.req.Name+".json")
	writeGitTestFile(t, sentinel, "outside file\n", 0o600)
	tx.rename = func(from, to string) error {
		if from == tx.stagedOrigin && to == tx.originPath {
			if err := os.Rename(tx.originParent, tx.originParent+"-kept"); err != nil {
				return err
			}
			if err := os.Symlink(outside, tx.originParent); err != nil {
				return err
			}
		}
		return move(from, to)
	}
	_, err := commitDependencyTestTransaction(t.Context(), tx)
	if err == nil || !strings.Contains(err.Error(), "joint restoration withheld") {
		t.Fatalf("parent substitution not detected: %v", err)
	}
	body, e := os.ReadFile(sentinel)
	if e != nil || string(body) != "outside file\n" {
		t.Fatalf("rename escaped to substituted parent: %q %v", body, e)
	}
	if _, e := os.Stat(tx.backupOrigin); e != nil {
		t.Fatalf("original sidecar not retained: %v", e)
	}
}

func TestAssistantDependencyTransactionRechecksAfterReferenceHook(t *testing.T) {
	f := newDependencyTransactionFixture(t, true, false)
	installOriginalDependencyOrigin(t, f)
	gitTestHook(t, f.root, "reference-transaction", `if [ "$1" = prepared ]; then
 printf 'changed after commit hooks\n' > .botz/.origins/shared-planner.json
fi`)
	_, err := assistantDependencyBotsUpdate(t.Context(), f.root, f.req)
	if err == nil || !strings.Contains(err.Error(), "joint restoration withheld") {
		t.Fatalf("reference hook interference published: %v", err)
	}
	if head := strings.TrimSpace(gitTestRead(t, f.root, "rev-parse", "HEAD")); head != f.head {
		t.Fatal("interfered candidate published")
	}
	body, e := os.ReadFile(filepath.Join(f.root, ".botz", ".origins", f.req.Name+".json"))
	if e != nil || string(body) != "changed after commit hooks\n" {
		t.Fatalf("reference hook edit lost: %q %v", body, e)
	}
}

func TestAssistantDependencyTransactionLinkedWorktree(t *testing.T) {
	f := newDependencyTransactionFixture(t, true, false)
	linked := filepath.Join(filepath.Dir(f.root), "linked")
	gitTestRead(t, f.root, "worktree", "add", "--detach", linked, "HEAD")
	t.Cleanup(func() { gittest.RemoveWorktree(t, f.root, linked) })
	response, err := assistantDependencyBotsUpdate(t.Context(), linked, f.req)
	if err != nil || response.Commit == "" {
		t.Fatalf("linked worktree update: %+v %v", response, err)
	}
	gitDir := strings.TrimSpace(gitTestRead(t, linked, "rev-parse", "--path-format=absolute", "--git-dir"))
	if paths, _ := filepath.Glob(filepath.Join(gitDir, "iterion-dependency-*")); len(paths) != 0 {
		t.Fatalf("per-worktree recovery leak: %v", paths)
	}
	// The original checkout retains its own lock/cache/index and branch.
	f.assertRestored(t, true)
}

func TestAssistantDependencyTransactionBackupsAreNotDiscoverable(t *testing.T) {
	f := newDependencyTransactionFixture(t, true, false)
	gitTestHook(t, f.root, "pre-commit", "printf 'operator cache\\n' > .botz/shared-planner/runtime.pyc\nexit 73")
	_, err := assistantDependencyBotsUpdate(t.Context(), f.root, f.req)
	if err == nil {
		t.Fatal("hook accepted")
	}
	backups, _ := filepath.Glob(filepath.Join(f.root, ".git", "iterion-dependency-*", "original-cache"))
	if len(backups) != 1 {
		t.Fatalf("missing recovery bundle: %v", backups)
	}
	entries, err := botregistry.List(botregistry.ListOptions{Workdir: f.root, Paths: botregistry.DefaultPaths(f.root)})
	if err != nil {
		t.Fatal(err)
	}
	selected := 0
	for _, entry := range entries {
		if entry.Name == f.req.Name {
			selected++
			if entry.Path != f.cache {
				t.Fatalf("discovered private backup: %+v", entry)
			}
		}
	}
	if selected != 1 {
		t.Fatalf("selected dependency has %d registry entries", selected)
	}
}
