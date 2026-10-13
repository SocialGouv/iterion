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
