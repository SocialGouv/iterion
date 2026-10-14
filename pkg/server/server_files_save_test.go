package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/gittest"
)

func saveWorkflowRequest(t *testing.T, s *Server, path, name string, createOnly bool) *httptest.ResponseRecorder {
	t.Helper()
	document := json.RawMessage(`{"workflows":[{"name":"` + name + `","entry":"done"}]}`)
	body, err := json.Marshal(saveFileRequest{
		Path:       path,
		Document:   document,
		CreateOnly: createOnly,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/files/save", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleSaveFile(rec, req)
	return rec
}

func TestSaveFileCreateOnlyCreatesNewWorkflow(t *testing.T) {
	workdir := t.TempDir()
	s := &Server{cfg: Config{WorkDir: workdir}}

	rec := saveWorkflowRequest(t, s, "reviewed.bot", "reviewed", true)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(workdir, "reviewed.bot"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("workflow reviewed:")) {
		t.Fatalf("saved source = %q", data)
	}
	var response saveFileResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode save response: %v", err)
	}
	if got, want := response.ConfirmedDiskPath, filepath.Join(workdir, "reviewed.bot"); got != want {
		t.Fatalf("confirmed_disk_path = %q, want %q", got, want)
	}
}

func TestOpenFileConfirmsDiskReadsButNotEmbeddedRecipes(t *testing.T) {
	workdir := t.TempDir()
	diskPath := filepath.Join(workdir, "disk.bot")
	if err := os.WriteFile(diskPath, []byte("workflow disk:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: Config{WorkDir: workdir}}

	open := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(openFileRequest{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/files/open", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleOpenFile(rec, req)
		return rec
	}

	disk := open("disk.bot")
	if disk.Code != http.StatusOK {
		t.Fatalf("disk open status = %d, body = %s", disk.Code, disk.Body.String())
	}
	var diskResponse struct {
		ConfirmedDiskPath string `json:"confirmed_disk_path"`
	}
	if err := json.Unmarshal(disk.Body.Bytes(), &diskResponse); err != nil {
		t.Fatal(err)
	}
	if got, want := diskResponse.ConfirmedDiskPath, diskPath; got != want {
		t.Fatalf("disk confirmed path = %q, want %q", got, want)
	}

	embedded := open("bots/feature-dev/main.bot")
	if embedded.Code != http.StatusOK {
		t.Fatalf("embedded open status = %d, body = %s", embedded.Code, embedded.Body.String())
	}
	var embeddedResponse struct {
		ConfirmedDiskPath string `json:"confirmed_disk_path"`
	}
	if err := json.Unmarshal(embedded.Body.Bytes(), &embeddedResponse); err != nil {
		t.Fatal(err)
	}
	if embeddedResponse.ConfirmedDiskPath != "" {
		t.Fatalf("embedded recipe was presented as disk-confirmed: %q", embeddedResponse.ConfirmedDiskPath)
	}
}

func TestSaveFileCreateOnlyCollisionPreservesExistingBytes(t *testing.T) {
	workdir := t.TempDir()
	path := filepath.Join(workdir, "existing.bot")
	original := []byte("operator-owned bytes\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: Config{WorkDir: workdir}}

	rec := saveWorkflowRequest(t, s, "existing.bot", "replacement", true)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s; want 409", rec.Code, rec.Body.String())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("collision changed existing file: %q", got)
	}
}

func TestSaveFileCreateOnlyConcurrentRequestsHaveOneWinner(t *testing.T) {
	workdir := t.TempDir()
	s := &Server{cfg: Config{WorkDir: workdir}}
	start := make(chan struct{})
	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for _, name := range []string{"first", "second"} {
		name := name
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes <- saveWorkflowRequest(t, s, "one-winner.bot", name, true).Code
		}()
	}
	close(start)
	wg.Wait()
	close(codes)

	counts := map[int]int{}
	for code := range codes {
		counts[code]++
	}
	if counts[http.StatusOK] != 1 || counts[http.StatusConflict] != 1 {
		t.Fatalf("status counts = %#v; want one 200 and one 409", counts)
	}
}

func TestSaveFileOrdinarySaveStillOverwrites(t *testing.T) {
	workdir := t.TempDir()
	path := filepath.Join(workdir, "existing.bot")
	if err := os.WriteFile(path, []byte("old bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: Config{WorkDir: workdir}}

	rec := saveWorkflowRequest(t, s, "existing.bot", "updated", false)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte("workflow updated:")) {
		t.Fatalf("ordinary save did not overwrite: %q", got)
	}
}

// Save As prepares the whole file privately before its exclusive publication;
// a preparation failure must not leave a partial .bot at the live path.
func TestWriteWorkflowFileCreateOnlyKeepsPreparationPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.bot")
	locks, err := acquireAuthoringLocalLocks(t.Context(), []string{path})
	if err != nil {
		t.Fatal(err)
	}
	defer closeAuthoringLocalLocks(locks)
	tx, err := prepareAuthoringLocal(locks[0], authoringPreviewFile{Operation: "create", After: "workflow partial:\n"}, nil, func(stage string) error {
		if stage == "record:prepared" {
			return errors.New("injected preparation failure")
		}
		return nil
	})
	if tx != nil {
		defer tx.close()
	}
	if err == nil {
		t.Fatal("expected injected failure")
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("partial workflow became visible: %v", statErr)
	}
}

// The studio's Workflow Settings inspector edits the workflow-level
// permission gate (mode + the allow/ask/deny lists). It promises a round
// trip: what the form writes into the document is what the `.bot` says
// afterwards. This exercises that promise at the seam the save crosses —
// a document in the studio's shape, carrying the four gate keys and
// nothing a parse would have added — and then the shape a cleared control
// produces, since `JSON.stringify` drops the `undefined` it writes.
func TestSaveFileCarriesTheWorkflowPermissionGate(t *testing.T) {
	workdir := t.TempDir()
	s := &Server{cfg: Config{WorkDir: workdir}}

	save := func(t *testing.T, document string) string {
		t.Helper()
		body, err := json.Marshal(saveFileRequest{Path: "gated.bot", Document: json.RawMessage(document)})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/files/save", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleSaveFile(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		data, err := os.ReadFile(filepath.Join(workdir, "gated.bot"))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	gated := save(t, `{"workflows":[{"name":"gated","entry":"done","permission":"ask",`+
		`"allow":["Read(**)"],"ask":["Bash(git push:*)"],"deny":["Bash(rm:*)"]}]}`)
	// One line per kind so a TRANSPORT key renamed or crossed names the kind
	// it broke: these four assertions are what reddens when the workflow's
	// `allow`/`ask`/`deny`/`permission` JSON tag changes. A writer that drops
	// a kind on the way out is caught upstream instead, by the status check
	// above — canon.Text reparses the saved source and ir.SameProgram refuses
	// the save with 422.
	for _, want := range []string{
		"permission: ask",
		`allow: ["Read(**)"]`,
		`ask: ["Bash(git push:*)"]`,
		`deny: ["Bash(rm:*)"]`,
	} {
		if !strings.Contains(gated, want) {
			t.Errorf("saved source is missing %q:\n%s", want, gated)
		}
	}

	cleared := save(t, `{"workflows":[{"name":"gated","entry":"done"}]}`)
	for _, gone := range []string{"permission:", "allow:", "ask:", "deny:"} {
		if strings.Contains(cleared, gone) {
			t.Errorf("saved source still declares %q after the controls were cleared:\n%s", gone, cleared)
		}
	}
}

// TestSaveFileRefusesAnEmptiedPermissionRuleList pins WHY an inspector control
// must write `undefined` for an emptied rule list rather than `[]`: the two
// are not the same value on this wire. `[]` unmarshals to a non-nil empty
// slice, ir.SameProgram compares the reparsed program with reflect.DeepEqual,
// and the save is refused — so a control that wrote `[]` would not "declare an
// empty list", it would make the file unsaveable, with a message that names no
// field (firstWorkflowDifference has no permission arm). An empty MODE is the
// other way round: the server drops it, and the save succeeds.
func TestSaveFileRefusesAnEmptiedPermissionRuleList(t *testing.T) {
	workdir := t.TempDir()
	s := &Server{cfg: Config{WorkDir: workdir}}

	post := func(t *testing.T, document string) (int, string) {
		t.Helper()
		body, err := json.Marshal(saveFileRequest{Path: "gate.bot", Document: json.RawMessage(document)})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/files/save", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleSaveFile(rec, req)
		return rec.Code, rec.Body.String()
	}

	// One case per kind AT EACH LEVEL: the six lists are independent, so a
	// tolerance added for one of them alone would leave the other five
	// refusing. Each case also asserts WHICH refusal it got — a 422 raised
	// for any other reason (an unparseable rule, a profile mismatch) would
	// otherwise keep the case green while the claim it witnesses was false.
	refused := func(t *testing.T, level, kind string, code int, body string) {
		t.Helper()
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("%s %s: [] saved with status %d (%s); an inspector that wrote [] for an emptied list would depend on this", level, kind, code, body)
		}
		if !strings.Contains(body, "not the same program") {
			t.Fatalf("%s %s: refused, but not as a program difference: %s", level, kind, body)
		}
	}
	for _, kind := range []string{"allow", "ask", "deny"} {
		t.Run("workflow_"+kind, func(t *testing.T) {
			code, body := post(t, `{"workflows":[{"name":"gate","entry":"done","`+kind+`":[]}]}`)
			refused(t, "workflow", kind, code, body)
		})
		t.Run("node_"+kind, func(t *testing.T) {
			code, body := post(t, `{"agents":[{"name":"start","model":"test-model","`+kind+`":[]}],`+
				`"workflows":[{"name":"gate","entry":"start","edges":[{"from":"start","to":"done"}]}]}`)
			refused(t, "node", kind, code, body)
		})
	}
	t.Run("mode_empty_string_is_accepted", func(t *testing.T) {
		code, body := post(t, `{"workflows":[{"name":"gate","entry":"done","permission":""}]}`)
		if code != http.StatusOK {
			t.Fatalf("an empty mode was refused: status %d (%s)", code, body)
		}
		data, err := os.ReadFile(filepath.Join(workdir, "gate.bot"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "permission:") {
			t.Fatalf("an empty mode was written to the source:\n%s", data)
		}
	})
}

// controlListingVictim is a private control entry of the shape a published
// transaction leaves behind, placed so a listing holds it and an inspection
// no longer finds it.
const controlListingVictim = "00000000-0000-4000-8000-000000000000.record"

// A save lists the private control directory of the file's own directory
// and inspects every entry it listed. A concurrent writer's transaction
// publishes its candidate, or drops its record, under that listing: the
// entry is gone by the time this save inspects it. That is not a reason to
// refuse the save, and above all not a reason to answer "not found" for a
// file that is there — the status the merge queue read.
func TestSaveFileCreateOnlyAnswersConflictWhenAListedControlEntryVanished(t *testing.T) {
	// One case per inspection of a listed entry: the two windows are
	// separate syscalls, and a tolerance added to one of them alone would
	// leave the other refusing.
	for _, window := range []struct {
		name string
		skip int
	}{
		{"the entry is gone before its stat", 0},
		{"the entry is gone before its open", 1},
	} {
		t.Run(window.name, func(t *testing.T) {
			workdir := t.TempDir()
			s := &Server{cfg: Config{WorkDir: workdir}}
			if rec := saveWorkflowRequest(t, s, "one-winner.bot", "winner", true); rec.Code != http.StatusOK {
				t.Fatalf("the first save did not build the control directory: %d %s", rec.Code, rec.Body.String())
			}
			victim := filepath.Join(workdir, ".iterion", "authoring", controlListingVictim)
			if err := os.WriteFile(victim, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			inspections := 0
			authoringListingHook = func(name string) {
				if name != controlListingVictim {
					return
				}
				inspections++
				if inspections != window.skip+1 {
					return
				}
				if err := os.Remove(victim); err != nil {
					t.Errorf("remove the listed entry: %v", err)
				}
			}
			t.Cleanup(func() { authoringListingHook = nil })

			rec := saveWorkflowRequest(t, s, "one-winner.bot", "loser", true)

			if inspections <= window.skip {
				t.Fatalf("the window never opened (%d inspections of %s): the test proves nothing", inspections, controlListingVictim)
			}
			// The refusal named, not just its status: a 409 raised for
			// another reason — the destination read as changed, the path
			// changed while waiting — would otherwise keep the case green.
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "file already exists") {
				t.Fatalf("status = %d, body = %s; want 409 for a file that exists", rec.Code, rec.Body.String())
			}
		})
	}
}

// The interleaving the merge queue hit, pinned: two create-only saves of
// one file, the loser holding the control listing it took while the winner
// publishes its candidate out of that directory. The loser answers 409 for
// the file the winner created, never 404.
func TestSaveFileCreateOnlyLoserAnswersConflictWhenTheWinnerPublishesUnderItsListing(t *testing.T) {
	workdir := t.TempDir()
	s := &Server{cfg: Config{WorkDir: workdir}}
	candidateStaged := make(chan struct{}) // the winner wrote its candidate and is about to publish
	listingTaken := make(chan struct{})    // the loser holds a listing that names that candidate
	winnerAnswered := make(chan struct{})  // the winner published and released its lock
	await := func(ch <-chan struct{}, what string) {
		select {
		case <-ch:
		case <-time.After(30 * time.Second):
			t.Errorf("the choreography never reached %s", what)
		}
	}
	var staged, listed sync.Once
	authoringStepHook = func(stage string) error {
		if stage != "publish" {
			return nil
		}
		staged.Do(func() { close(candidateStaged) })
		await(listingTaken, "the loser's listing")
		return nil
	}
	authoringListingHook = func(name string) {
		if !strings.HasSuffix(name, ".candidate") {
			return
		}
		listed.Do(func() { close(listingTaken) })
		await(winnerAnswered, "the winner's answer")
	}
	t.Cleanup(func() { authoringStepHook = nil; authoringListingHook = nil })

	winner := make(chan int, 1)
	go func() {
		winner <- saveWorkflowRequest(t, s, "one-winner.bot", "winner", true).Code
		close(winnerAnswered)
	}()
	// Whatever happens next, the winner is released and joined before this
	// test completes: a choreography that breaks down — the loser refused
	// before it ever lists, every pre-listing refusal of the save — leaves
	// the winner parked in the step hook, and a park that outlives the test
	// reports on a completed *testing.T, which aborts the whole binary and
	// takes every later verdict of the package with it. Registered after the
	// hooks (t.Cleanup is LIFO) so it runs before they are cleared, and
	// after the goroutine exists so the join cannot wait on nothing.
	t.Cleanup(func() {
		listed.Do(func() { close(listingTaken) })
		<-winnerAnswered
	})
	await(candidateStaged, "the winner's staged candidate")
	loser := saveWorkflowRequest(t, s, "one-winner.bot", "loser", true)

	select {
	case <-listingTaken:
	default:
		t.Fatal("the loser never listed the winner's candidate: the test proves nothing")
	}
	if code := <-winner; code != http.StatusOK {
		t.Fatalf("winner status = %d", code)
	}
	if loser.Code != http.StatusConflict || !strings.Contains(loser.Body.String(), "file already exists") {
		t.Fatalf("loser status = %d, body = %s; want 409 naming the collision", loser.Code, loser.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(workdir, "one-winner.bot"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("workflow winner:")) {
		t.Fatalf("the loser overwrote the winner's file: %q", data)
	}
}

// The recovery directory is resolved through symlinks on purpose — a save
// in place may keep its locks elsewhere — so an entry pointing nowhere is
// a layout, not an absence: no retry repairs it, and the file being saved
// is there all along. The refusal names the entry an operator can see, not
// the path it points at, and never answers for the file.
func TestSaveFileRefusesARecoveryDirectoryThatResolvesToNothing(t *testing.T) {
	workdir := t.TempDir()
	s := &Server{cfg: Config{WorkDir: workdir}}
	destination := filepath.Join(workdir, "one-winner.bot")
	if err := os.WriteFile(destination, []byte("operator-owned bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(workdir, "no-such-recovery"), filepath.Join(workdir, ".iterion")); err != nil {
		t.Fatal(err)
	}

	rec := saveWorkflowRequest(t, s, "one-winner.bot", "loser", true)

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unsafe authoring directory") {
		t.Fatalf("status = %d, body = %s; want 400 for a recovery directory that resolves to nothing", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "no-such-recovery") {
		t.Fatalf("the refusal names where the entry points instead of the entry: %s", rec.Body.String())
	}
	data, err := os.ReadFile(destination)
	if err != nil || string(data) != "operator-owned bytes\n" {
		t.Fatalf("destination = %q, %v; want the operator's bytes, untouched", data, err)
	}
	// A layout, not a transient state: the same request answers the same
	// way until the operator repairs the entry.
	if again := saveWorkflowRequest(t, s, "one-winner.bot", "loser", true); again.Code != http.StatusBadRequest {
		t.Fatalf("retried status = %d, body = %s; want the same refusal", again.Code, again.Body.String())
	}
}

// Tolerating an entry that is no longer there is exactly that: an entry
// that IS there is still held to everything the audit asks of its kind —
// an expected name, of every entry; a regular file, of every entry but a
// displaced copy, a candidate included; and of the three the audit opens,
// the exclusion, a lock and a record, a file it can read and one only this
// server may read and write. The exclusion's own gate is the one re-run
// under the lock, where the next bytes of recovery data are written beside
// it; the rows here are the refusals this audit owns, one case each, so
// each dies alone.
func TestSaveFileRefusesAControlDirectoryHoldingAnUnexpectedOrUnsafeEntry(t *testing.T) {
	for _, entry := range []struct {
		name, want string
		plant      func(t *testing.T, control string)
	}{
		{
			name: "an unknown name",
			want: "unexpected authoring control entry",
			plant: func(t *testing.T, control string) {
				if err := os.WriteFile(filepath.Join(control, "intruder"), []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a private file that is not regular",
			want: "unsafe authoring control file",
			plant: func(t *testing.T, control string) {
				if err := os.Symlink(filepath.Join(control, "no-such-target"), filepath.Join(control, "00000000-0000-4000-8000-000000000001.record")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// A candidate is a kind this audit opens nothing of, and it
			// is still held to being a regular file: the entry a save
			// publishes into the live path is never followed.
			name: "a candidate that is not regular",
			want: "unsafe authoring control file",
			plant: func(t *testing.T, control string) {
				if err := os.Symlink(filepath.Join(control, "no-such-target"), filepath.Join(control, "00000000-0000-4000-8000-000000000004.candidate")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// The tolerance is for an entry that is NOT THERE, not for
			// one the audit cannot inspect: this one exists and refuses
			// to be read, and the save is refused with it.
			name: "a private file the audit cannot inspect",
			want: "permission denied",
			plant: func(t *testing.T, control string) {
				path := filepath.Join(control, "00000000-0000-4000-8000-000000000002.record")
				if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o000); err != nil {
					t.Fatal(err)
				}
				if f, err := os.Open(path); err == nil {
					_ = f.Close()
					t.Skip("this user reads a file with no permission bits: nothing to refuse")
				}
			},
		},
		{
			name: "a private file others may read",
			want: "unsafe authoring ownership or permissions",
			plant: func(t *testing.T, control string) {
				path := filepath.Join(control, "00000000-0000-4000-8000-000000000003.record")
				if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// Another save's lock, which this one never takes: the
			// audit answers for every lock the directory holds, not
			// only for the one this save is about to flock.
			name: "a sibling lock others may read",
			want: "unsafe authoring ownership or permissions",
			plant: func(t *testing.T, control string) {
				path := filepath.Join(control, strings.Repeat("ab", 32)+".lock")
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(entry.name, func(t *testing.T) {
			workdir := t.TempDir()
			s := &Server{cfg: Config{WorkDir: workdir}}
			if rec := saveWorkflowRequest(t, s, "one-winner.bot", "winner", true); rec.Code != http.StatusOK {
				t.Fatalf("the first save did not build the control directory: %d %s", rec.Code, rec.Body.String())
			}
			entry.plant(t, filepath.Join(workdir, ".iterion", "authoring"))

			rec := saveWorkflowRequest(t, s, "one-winner.bot", "loser", true)

			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), entry.want) {
				t.Fatalf("status = %d, body = %s; want 400 naming %q", rec.Code, rec.Body.String(), entry.want)
			}
		})
	}
}

// The kind the audit asks only a name of: a displaced copy is the live
// file of a save mid-flight, moved aside and read back by the transaction
// that put it there, through the gate that pins its inode and its mode. So
// a control directory holding one carries another save, whatever that
// entry is and whoever may read it — an audit widened to that kind would
// refuse every save made while a neighbour is between its displacement and
// its commit.
func TestSaveFileAcceptsADisplacedCopyTheAuditAsksOnlyANameOf(t *testing.T) {
	for _, entry := range []struct {
		name  string
		plant func(t *testing.T, control string)
	}{
		{
			// Private, and not a regular file: only the exemption of
			// its kind can carry this one past the audit.
			name: "a displaced copy that is not a regular file",
			plant: func(t *testing.T, control string) {
				path := filepath.Join(control, "00000000-0000-4000-8000-000000000005.before")
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// A regular file others may read: the mode a displaced copy
			// carries whenever the live file it moved aside had it.
			name: "a displaced copy others may read",
			plant: func(t *testing.T, control string) {
				path := filepath.Join(control, "00000000-0000-4000-8000-000000000006.undo")
				if err := os.WriteFile(path, []byte("displaced\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(entry.name, func(t *testing.T) {
			workdir := t.TempDir()
			s := &Server{cfg: Config{WorkDir: workdir}}
			if rec := saveWorkflowRequest(t, s, "one-winner.bot", "winner", true); rec.Code != http.StatusOK {
				t.Fatalf("the first save did not build the control directory: %d %s", rec.Code, rec.Body.String())
			}
			entry.plant(t, filepath.Join(workdir, ".iterion", "authoring"))

			rec := saveWorkflowRequest(t, s, "second.bot", "second", true)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s; want 200 for a save whose control directory holds a displaced copy", rec.Code, rec.Body.String())
			}
			data, err := os.ReadFile(filepath.Join(workdir, "second.bot"))
			if err != nil || !bytes.Contains(data, []byte("workflow second:")) {
				t.Fatalf("second.bot = %q, %v; want the bytes of a save the audit let through", data, err)
			}
		})
	}
}

// The exclusion is read where it is created or found, before the listing:
// the step above answered for its presence — it created the file, or found
// it there — so an exclusion gone by the read is another writer's removal.
// The same conflict the locked re-read answers, one site earlier — never a
// 404, which would say the file being SAVED is not there, and it is.
func TestSaveFileRefusesAnExclusionThatVanishedBeforeItWasRead(t *testing.T) {
	workdir := t.TempDir()
	s := &Server{cfg: Config{WorkDir: workdir}}
	if rec := saveWorkflowRequest(t, s, "one-winner.bot", "winner", true); rec.Code != http.StatusOK {
		t.Fatalf("the first save did not build the control directory: %d %s", rec.Code, rec.Body.String())
	}
	exclusion := filepath.Join(workdir, ".iterion", "authoring", ".gitignore")
	removed := false
	authoringExclusionHook = func() {
		if removed {
			return
		}
		removed = true
		if err := os.Remove(exclusion); err != nil {
			t.Errorf("remove the exclusion the save just found: %v", err)
		}
	}
	t.Cleanup(func() { authoringExclusionHook = nil })

	rec := saveWorkflowRequest(t, s, "one-winner.bot", "loser", true)

	if !removed {
		t.Fatal("the exclusion was never read: the test proves nothing")
	}
	// The destination of the refused save, read back: a 404 answers for
	// THIS file, and these are the winner's bytes, untouched.
	data, err := os.ReadFile(filepath.Join(workdir, "one-winner.bot"))
	if err != nil || !bytes.Contains(data, []byte("workflow winner:")) {
		t.Fatalf("the file the save was refused for = %q, %v; want the winner's bytes", data, err)
	}
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "vanished while opening the control directory") {
		t.Fatalf("status = %d, body = %s; want 409 naming the vanished exclusion", rec.Code, rec.Body.String())
	}
}

// The tolerance covers a transient entry, not the exclusion file the
// control directory must keep: a save whose `.gitignore` goes missing in
// that same window is refused once the lock is held — as the conflict it
// is, naming the exclusion. Never as a collision with the file it is
// saving, which is what a save that went through would have said, and
// never as that file's absence, which is what the bare ENOENT answers.
func TestSaveFileRefusesWhenTheControlExclusionVanishedFromTheListing(t *testing.T) {
	workdir := t.TempDir()
	s := &Server{cfg: Config{WorkDir: workdir}}
	if rec := saveWorkflowRequest(t, s, "one-winner.bot", "winner", true); rec.Code != http.StatusOK {
		t.Fatalf("the first save did not build the control directory: %d %s", rec.Code, rec.Body.String())
	}
	exclusion := filepath.Join(workdir, ".iterion", "authoring", ".gitignore")
	removed := false
	authoringListingHook = func(name string) {
		if name != ".gitignore" || removed {
			return
		}
		removed = true
		if err := os.Remove(exclusion); err != nil {
			t.Errorf("remove the listed exclusion file: %v", err)
		}
	}
	t.Cleanup(func() { authoringListingHook = nil })

	rec := saveWorkflowRequest(t, s, "one-winner.bot", "loser", true)

	if !removed {
		t.Fatal("the exclusion file was never listed: the test proves nothing")
	}
	// Refused FOR the exclusion: the answer names it, and says the state
	// it found — a directory that lost the rule keeping its recovery data
	// private, which a retry recreates. A 404 would say the file being
	// saved is not there, and it is; a 409 "file already exists" would be
	// the answer of a save that went through.
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), ".gitignore") {
		t.Fatalf("status = %d, body = %s; want 409 naming the vanished exclusion", rec.Code, rec.Body.String())
	}
}

// An entry the audit cannot inspect is not an entry that is not there: a
// control directory that stops answering while it is being listed refuses
// the save. It does not pass for empty — which is what a tolerance of any
// inspection failure, rather than of absence alone, would make it do.
func TestSaveFileRefusesAControlDirectoryItCannotInspect(t *testing.T) {
	workdir := t.TempDir()
	s := &Server{cfg: Config{WorkDir: workdir}}
	if rec := saveWorkflowRequest(t, s, "one-winner.bot", "winner", true); rec.Code != http.StatusOK {
		t.Fatalf("the first save did not build the control directory: %d %s", rec.Code, rec.Body.String())
	}
	control := filepath.Join(workdir, ".iterion", "authoring")
	t.Cleanup(func() { _ = os.Chmod(control, 0o700) })
	if err := os.Chmod(control, 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(control, ".gitignore")); err == nil {
		t.Skip("this user searches a directory with no permission bits: nothing to refuse")
	}
	if err := os.Chmod(control, 0o700); err != nil {
		t.Fatal(err)
	}
	closed := false
	authoringListingHook = func(name string) {
		switch {
		case name != "" && !closed:
			closed = true
			if err := os.Chmod(control, 0o000); err != nil {
				t.Errorf("close the control directory under its listing: %v", err)
			}
		case name == "":
			// Only reached when the audit carried on past the entry it
			// could not inspect: the save then needs its directory back.
			if err := os.Chmod(control, 0o700); err != nil {
				t.Errorf("reopen the control directory: %v", err)
			}
		}
	}
	t.Cleanup(func() { authoringListingHook = nil })

	rec := saveWorkflowRequest(t, s, "second.bot", "second", true)

	if !closed {
		t.Fatal("the listing inspected no entry: the test proves nothing")
	}
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "permission denied") {
		t.Fatalf("status = %d, body = %s; want 400 for a directory the audit cannot inspect", rec.Code, rec.Body.String())
	}
}

// The other entry the audit may skip is a lock file, and the save that
// takes one answers for it itself, after the listing: a lock opened to
// others between the audit and the save that takes it refuses the save.
func TestSaveFileRefusesALockItCannotAnswerFor(t *testing.T) {
	workdir := t.TempDir()
	s := &Server{cfg: Config{WorkDir: workdir}}
	if rec := saveWorkflowRequest(t, s, "one-winner.bot", "winner", true); rec.Code != http.StatusOK {
		t.Fatalf("the first save did not build the control directory: %d %s", rec.Code, rec.Body.String())
	}
	control := filepath.Join(workdir, ".iterion", "authoring")
	lock := ""
	authoringListingHook = func(name string) {
		if strings.HasSuffix(name, ".lock") {
			lock = name
			return
		}
		if name != "" || lock == "" {
			return
		}
		if err := os.Chmod(filepath.Join(control, lock), 0o644); err != nil {
			t.Errorf("open the audited lock to others: %v", err)
		}
	}
	t.Cleanup(func() { authoringListingHook = nil })

	rec := saveWorkflowRequest(t, s, "one-winner.bot", "loser", true)

	if lock == "" {
		t.Fatal("the listing held no lock file: the test proves nothing")
	}
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unsafe authoring ownership or permissions") {
		t.Fatalf("status = %d, body = %s; want 400 for a lock the save cannot answer for", rec.Code, rec.Body.String())
	}
}

// The audit answers for a directory, and this save's own lock is created
// in it one step later: a control directory removed in between is linked
// nowhere, and nothing can be created in it. That is this save's conflict,
// named — the open's own error names neither the directory nor any path,
// and as a bare ENOENT it answers 404 for a file that is there.
func TestSaveFileRefusesAControlDirectoryThatVanishedAfterItsAudit(t *testing.T) {
	workdir := t.TempDir()
	s := &Server{cfg: Config{WorkDir: workdir}}
	if rec := saveWorkflowRequest(t, s, "one-winner.bot", "winner", true); rec.Code != http.StatusOK {
		t.Fatalf("the first save did not build the control directory: %d %s", rec.Code, rec.Body.String())
	}
	// Built by the save above, inside this test's own temp root.
	control := filepath.Join(workdir, ".iterion", "authoring")
	removed := false
	authoringListingHook = func(name string) {
		if name != "" || removed {
			return
		}
		removed = true
		if err := os.RemoveAll(control); err != nil {
			t.Errorf("remove the audited control directory: %v", err)
		}
	}
	t.Cleanup(func() { authoringListingHook = nil })

	rec := saveWorkflowRequest(t, s, "one-winner.bot", "loser", true)

	if !removed {
		t.Fatal("the audit never ended: the test proves nothing")
	}
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "control directory vanished while taking the lock") {
		t.Fatalf("status = %d, body = %s; want 409 naming the vanished control directory", rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(workdir, "one-winner.bot"))
	if err != nil || !bytes.Contains(data, []byte("workflow winner:")) {
		t.Fatalf("the file the save was refused for = %q, %v; want the winner's bytes", data, err)
	}
	// What makes it a conflict and not a layout: the next save rebuilds
	// the directory it needs and goes through.
	if again := saveWorkflowRequest(t, s, "second.bot", "second", true); again.Code != http.StatusOK {
		t.Fatalf("retried status = %d, body = %s; want 200 from a save that rebuilds the control directory", again.Code, again.Body.String())
	}
}

// A control directory the audit passed is not a directory that stays that
// way: the exclusion file is read again with the lock held, where the next
// bytes of recovery data are about to be written beside it, and held to
// the ownership every private control file must have. An exclusion opened
// to others after the audit — by the process that hid it from its own
// window, or by any other — never carries a save.
func TestSaveFileRefusesAnExclusionTheAuditNoLongerAnswersFor(t *testing.T) {
	workdir := t.TempDir()
	s := &Server{cfg: Config{WorkDir: workdir}}
	if rec := saveWorkflowRequest(t, s, "one-winner.bot", "winner", true); rec.Code != http.StatusOK {
		t.Fatalf("the first save did not build the control directory: %d %s", rec.Code, rec.Body.String())
	}
	exclusion := filepath.Join(workdir, ".iterion", "authoring", ".gitignore")
	audited := false
	authoringListingHook = func(name string) {
		if name != "" || audited {
			return
		}
		audited = true
		if err := os.Chmod(exclusion, 0o644); err != nil {
			t.Errorf("open the audited exclusion to others: %v", err)
		}
	}
	t.Cleanup(func() { authoringListingHook = nil })

	rec := saveWorkflowRequest(t, s, "second.bot", "second", true)

	if !audited {
		t.Fatal("the audit never ended: the test proves nothing")
	}
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unsafe authoring ownership or permissions") {
		t.Fatalf("status = %d, body = %s; want 400 for an exclusion the save cannot answer for", rec.Code, rec.Body.String())
	}
	if _, err := os.Lstat(filepath.Join(workdir, "second.bot")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the refused save wrote its file anyway: %v", err)
	}
}

// The exclusion is answered for by what it HOLDS too, not only by who may
// read it: a `.gitignore` rewritten between the audit and the lock — with
// its private mode untouched, so no ownership gate has anything to say
// about it — carries no save. The recovery copies about to be written
// beside it would be ordinary files of the user's repository, which is
// what the rule exists to prevent.
func TestSaveFileRefusesAnExclusionThatNoLongerExcludes(t *testing.T) {
	workdir := t.TempDir()
	s := &Server{cfg: Config{WorkDir: workdir}}
	if rec := saveWorkflowRequest(t, s, "one-winner.bot", "winner", true); rec.Code != http.StatusOK {
		t.Fatalf("the first save did not build the control directory: %d %s", rec.Code, rec.Body.String())
	}
	exclusion := filepath.Join(workdir, ".iterion", "authoring", ".gitignore")
	audited := false
	authoringListingHook = func(name string) {
		if name != "" || audited {
			return
		}
		audited = true
		// os.WriteFile truncates an existing file without touching its
		// mode, so the exclusion stays 0600 and only its content changed.
		if err := os.WriteFile(exclusion, []byte("# nothing is excluded\n"), 0o600); err != nil {
			t.Errorf("rewrite the audited exclusion: %v", err)
		}
	}
	t.Cleanup(func() { authoringListingHook = nil })

	rec := saveWorkflowRequest(t, s, "second.bot", "second", true)

	if !audited {
		t.Fatal("the audit never ended: the test proves nothing")
	}
	if info, err := os.Lstat(exclusion); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the rewritten exclusion is not the private file the ownership gate accepts: %v %v", info, err)
	}
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "exclusion changed while acquiring lock") {
		t.Fatalf("status = %d, body = %s; want 409 for an exclusion that no longer excludes", rec.Code, rec.Body.String())
	}
	if _, err := os.Lstat(filepath.Join(workdir, "second.bot")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the refused save wrote its file anyway: %v", err)
	}
}

// A listing holds what the directory holds, and a save mid-flight holds a
// staged candidate there: the bytes of the file it is about to become,
// carrying that file's mode — 0644 for a new workflow — not the private
// mode of the directory's own bookkeeping. So the audit opens and owns the
// entries that have a reader in this save (the exclusion, a lock, a
// record) and leaves a candidate to the transaction that staged it: a save
// whose listing names a neighbour's candidate goes through. An audit
// widened to that kind would refuse every save made while another save of
// the same directory is mid-flight.
func TestSaveFileAcceptsTheCandidateANeighbourSaveStaged(t *testing.T) {
	workdir := t.TempDir()
	s := &Server{cfg: Config{WorkDir: workdir}}
	control := filepath.Join(workdir, ".iterion", "authoring")
	candidateStaged := make(chan struct{})   // the parked save staged its candidate and is about to publish
	neighbourAnswered := make(chan struct{}) // the other save answered; the parked one may publish
	parkedAnswered := make(chan struct{})    // the parked save published and released its lock
	await := func(ch <-chan struct{}, what string) {
		select {
		case <-ch:
		case <-time.After(30 * time.Second):
			t.Errorf("the choreography never reached %s", what)
		}
	}
	var mu sync.Mutex
	neighbourRunning := false
	seen := false
	perm := os.FileMode(0)
	var staged, released sync.Once
	authoringStepHook = func(stage string) error {
		if stage != "publish" {
			return nil
		}
		// The neighbour publishes too, and parking it would deadlock on
		// its own answer: only the save started before it is held.
		mu.Lock()
		held := !neighbourRunning
		mu.Unlock()
		if !held {
			return nil
		}
		staged.Do(func() { close(candidateStaged) })
		await(neighbourAnswered, "the neighbour's answer")
		return nil
	}
	authoringListingHook = func(name string) {
		if !strings.HasSuffix(name, ".candidate") {
			return
		}
		info, err := os.Lstat(filepath.Join(control, name))
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			t.Errorf("stat the listed candidate: %v", err)
			return
		}
		seen, perm = true, info.Mode().Perm()
	}
	t.Cleanup(func() { authoringStepHook = nil; authoringListingHook = nil })

	parked := make(chan int, 1)
	go func() {
		parked <- saveWorkflowRequest(t, s, "one-winner.bot", "winner", true).Code
		close(parkedAnswered)
	}()
	// Whatever happens next, the parked save is released and joined before
	// this test completes: a park that outlives the test reports on a
	// completed *testing.T, which aborts the whole binary and takes every
	// later verdict of the package with it. Registered after the hooks
	// (t.Cleanup is LIFO) so it runs before they are cleared, and after the
	// goroutine exists so the join cannot wait on nothing.
	t.Cleanup(func() {
		released.Do(func() { close(neighbourAnswered) })
		<-parkedAnswered
	})
	await(candidateStaged, "the staged candidate")
	mu.Lock()
	neighbourRunning = true
	mu.Unlock()

	neighbour := saveWorkflowRequest(t, s, "neighbour.bot", "neighbour", true)
	released.Do(func() { close(neighbourAnswered) })

	mu.Lock()
	sawCandidate, candidatePerm := seen, perm
	mu.Unlock()
	if !sawCandidate {
		t.Fatal("the neighbour's listing never held the staged candidate: the test proves nothing")
	}
	// The mode is the whole reason that entry cannot be owned by this
	// audit: a private file is one no other user may read, and a candidate
	// is already the public file it is about to become.
	if candidatePerm&0o077 == 0 {
		t.Fatalf("the staged candidate is private (%v): an audit could hold it to that mode, and this test proves nothing", candidatePerm)
	}
	if candidatePerm != 0o644 {
		t.Fatalf("the staged candidate of a new workflow = %v; want 0644, the mode of the file it becomes", candidatePerm)
	}
	if neighbour.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s; want 200 for a save whose listing holds a neighbour's staged candidate", neighbour.Code, neighbour.Body.String())
	}
	if code := <-parked; code != http.StatusOK {
		t.Fatalf("the parked save answered %d", code)
	}
	for name, want := range map[string]string{"one-winner.bot": "workflow winner:", "neighbour.bot": "workflow neighbour:"} {
		data, err := os.ReadFile(filepath.Join(workdir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(data, []byte(want)) {
			t.Fatalf("%s = %q; want it to carry %q", name, data, want)
		}
	}
}

// The exclusion the control directory must keep is a Git fact too, and it
// is verified again once the lock is held: a directory whose private
// exclusion Git starts tracking while this save waits for its lock carries
// no save either. Its recovery copies of the user's files would be
// committable — the very layout the pre-listing verification refuses, on a
// directory that was still clean when it ran.
func TestSaveFileRefusesAControlDirectoryGitStartedTracking(t *testing.T) {
	root := gittest.SourceRepo(t)
	s := &Server{cfg: Config{WorkDir: root}}
	if rec := saveWorkflowRequest(t, s, "one-winner.bot", "winner", true); rec.Code != http.StatusOK {
		t.Fatalf("the first save did not build the control directory: %d %s", rec.Code, rec.Body.String())
	}
	staged := false
	authoringListingHook = func(name string) {
		if name != "" || staged {
			return
		}
		staged = true
		gittest.Run(t, root, "add", "-f", filepath.Join(".iterion", "authoring", ".gitignore"))
	}
	t.Cleanup(func() { authoringListingHook = nil })

	rec := saveWorkflowRequest(t, s, "second.bot", "second", true)

	if !staged {
		t.Fatal("the audit never ended: the test proves nothing")
	}
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "tracked or staged files") {
		t.Fatalf("status = %d, body = %s; want 400 for a control directory Git started tracking", rec.Code, rec.Body.String())
	}
	if _, err := os.Lstat(filepath.Join(root, "second.bot")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the refused save wrote its file anyway: %v", err)
	}
}
