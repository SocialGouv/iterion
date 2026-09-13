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
