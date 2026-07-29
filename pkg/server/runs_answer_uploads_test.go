package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
)

// stageOne uploads a file through the real handler and returns its
// staged upload id, so these tests exercise the same staging artefacts
// the studio produces rather than hand-built fixtures.
func stageOne(t *testing.T, srv *Server, filename, mime string, body []byte) string {
	t.Helper()
	buf, ct := multipartBody(t, filename, mime, body)
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/runs/uploads", buf)
	r.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	srv.handleUploadAttachment(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("upload %q: status %d (%s)", filename, w.Code, w.Body.String())
	}
	var staged stagedUpload
	if err := json.NewDecoder(w.Body).Decode(&staged); err != nil {
		t.Fatalf("decode staged upload: %v", err)
	}
	return staged.UploadID
}

func answerUploadTestServer(t *testing.T) *Server {
	t.Helper()
	srv := uploadTestServer(t)
	srv.cfg.AllowedUploadMIMEs = []string{"text/*", "image/*", "audio/*", "application/octet-stream"}
	srv.cfg.MaxUploadsPerRun = 20
	return srv
}

func TestAsUploadEnvelope(t *testing.T) {
	tests := []struct {
		name string
		val  any
		want string
	}{
		{"envelope", map[string]any{"upload_id": "up_123"}, "up_123"},
		{"envelope with filename hint", map[string]any{"upload_id": "up_123", "filename": "a.mp3"}, "up_123"},
		{"plain string is not an envelope", "up_123", ""},
		{"empty id rejected", map[string]any{"upload_id": ""}, ""},
		{"non-string id rejected", map[string]any{"upload_id": 42}, ""},
		// The structural match must not swallow a legitimate `json`
		// field that happens to carry an upload_id among other keys.
		{"extra keys make it ordinary data", map[string]any{"upload_id": "up_123", "other": 1}, ""},
		{"unrelated map", map[string]any{"path": "/tmp/x"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := asUploadEnvelope(tc.val)
			if tc.want == "" {
				if ok {
					t.Errorf("asUploadEnvelope(%v) = %q, want not-an-envelope", tc.val, got)
				}
				return
			}
			if !ok || got != tc.want {
				t.Errorf("asUploadEnvelope(%v) = (%q,%v), want (%q,true)", tc.val, got, ok, tc.want)
			}
		})
	}
}

func TestGateAttachmentNameIsPredictableAndCollisionSafe(t *testing.T) {
	taken := map[string]bool{}

	first := gateAttachmentName("gate_music", "music", taken)
	if first != "gate_music.music" {
		t.Errorf("first = %q, want gate_music.music (author-predictable)", first)
	}
	taken[first] = true

	// Re-answering the same gate (a rejected verdict looped back) must
	// not clobber the earlier upload.
	second := gateAttachmentName("gate_music", "music", taken)
	if second != "gate_music.music-2" {
		t.Errorf("second = %q, want gate_music.music-2", second)
	}

	// Path separators and other hostile characters never survive into a
	// name that is later used as a directory component.
	got := gateAttachmentName("../../etc", "pass/wd", map[string]bool{})
	if got == "" || filepath.Base(got) != got || got == "../../etc" {
		t.Errorf("sanitised name = %q, want a single safe component", got)
	}
}

func TestPromoteAnswerUploads_DeclaredFileField(t *testing.T) {
	srv := answerUploadTestServer(t)
	runID := "run-answer-file"
	seedRun(t, srv, runID, "wf", store.RunStatusPausedWaitingHuman)

	uploadID := stageOne(t, srv, "track.txt", "text/plain", []byte("fake audio bytes"))

	answers := map[string]any{
		"approved": true,
		"music":    map[string]any{"upload_id": uploadID},
		"notes":    "use this take",
	}
	out, err := srv.promoteAnswerUploads(context.Background(), runID, "gate_music", answers, nil)
	if err != nil {
		t.Fatalf("promoteAnswerUploads: %v", err)
	}

	desc, ok := out["music"].(map[string]any)
	if !ok {
		t.Fatalf("music = %T, want a descriptor map", out["music"])
	}
	if got := desc["attachment"]; got != "gate_music.music" {
		t.Errorf("attachment = %v, want gate_music.music", got)
	}
	if got := desc["filename"]; got != "track.txt" {
		t.Errorf("filename = %v, want track.txt", got)
	}
	if desc["sha256"] == "" || desc["sha256"] == nil {
		t.Error("descriptor is missing the server-computed sha256")
	}
	// path is the RUNTIME's job (host vs container) — the server must
	// not guess it.
	if _, present := desc["path"]; present {
		t.Error("server must not set path; the runtime resolves it sandbox-aware")
	}
	// Non-file answers pass through untouched.
	if out["approved"] != true || out["notes"] != "use this take" {
		t.Errorf("ordinary answers were altered: %+v", out)
	}

	// The bytes are a real run attachment, readable through the store.
	rc, rec, err := srv.runs.OpenAttachment(context.Background(), runID, "gate_music.music")
	if err != nil {
		t.Fatalf("OpenAttachment: %v", err)
	}
	defer rc.Close()
	if rec.Size == 0 {
		t.Error("promoted attachment is empty")
	}
}

func TestPromoteAnswerUploads_AdHocAttachments(t *testing.T) {
	srv := answerUploadTestServer(t)
	runID := "run-answer-adhoc"
	seedRun(t, srv, runID, "wf", store.RunStatusPausedWaitingHuman)

	a := stageOne(t, srv, "sketch.txt", "text/plain", []byte("a diagram"))
	b := stageOne(t, srv, "notes.txt", "text/plain", []byte("more context"))

	answers := map[string]any{"feedback": "see attached"}
	out, err := srv.promoteAnswerUploads(context.Background(), runID, "review_gate", answers, []string{a, b})
	if err != nil {
		t.Fatalf("promoteAnswerUploads: %v", err)
	}

	list, ok := out[answerUploadsKey].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("%s = %#v, want 2 descriptors", answerUploadsKey, out[answerUploadsKey])
	}
	for i, item := range list {
		desc, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("descriptor %d is %T", i, item)
		}
		if desc["attachment"] == "" || desc["filename"] == "" {
			t.Errorf("descriptor %d incomplete: %+v", i, desc)
		}
	}
	if out["feedback"] != "see attached" {
		t.Errorf("feedback altered: %v", out["feedback"])
	}
}

// A resume that references a nonexistent upload must fail cleanly and
// leave the run with no attachments — never half-populated.
func TestPromoteAnswerUploads_RollsBackOnBadUpload(t *testing.T) {
	srv := answerUploadTestServer(t)
	runID := "run-answer-rollback"
	seedRun(t, srv, runID, "wf", store.RunStatusPausedWaitingHuman)

	good := stageOne(t, srv, "good.txt", "text/plain", []byte("fine"))

	answers := map[string]any{
		"a": map[string]any{"upload_id": good},
		"b": map[string]any{"upload_id": "up_does_not_exist"},
	}
	if _, err := srv.promoteAnswerUploads(context.Background(), runID, "gate", answers, nil); err == nil {
		t.Fatal("expected an error for a missing upload")
	}

	list, err := srv.runs.ListAttachments(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("rollback left %d attachment(s) behind: %+v", len(list), list)
	}
	// The operator's original answers must be untouched so the caller can
	// surface them back for a retry.
	if _, stillEnvelope := asUploadEnvelope(answers["a"]); !stillEnvelope {
		t.Error("input answers were mutated despite the failure")
	}
}

// No uploads anywhere → the map is returned as-is, no store round-trip.
func TestPromoteAnswerUploads_NoUploadsIsAPassthrough(t *testing.T) {
	srv := answerUploadTestServer(t)
	answers := map[string]any{"approved": true, "notes": "lgtm"}

	out, err := srv.promoteAnswerUploads(context.Background(), "run-none", "gate", answers, nil)
	if err != nil {
		t.Fatalf("promoteAnswerUploads: %v", err)
	}
	if len(out) != 2 || out["approved"] != true {
		t.Errorf("passthrough altered answers: %+v", out)
	}
	if hasUploadEnvelope(answers) {
		t.Error("hasUploadEnvelope should be false for upload-free answers")
	}
}

func TestPromoteAnswerUploads_RejectsTooManyAdHoc(t *testing.T) {
	srv := answerUploadTestServer(t)
	ids := make([]string, maxAdHocGateUploads+1)
	for i := range ids {
		ids[i] = "up_placeholder"
	}
	if _, err := srv.promoteAnswerUploads(context.Background(), "run-x", "gate", nil, ids); err == nil {
		t.Fatal("expected a rejection above the per-answer cap")
	}
}

func TestDefaultMIMEAllowlistCoversMedia(t *testing.T) {
	allow := defaultAllowedMIMEs()
	// The soundtrack case that motivated gate uploads: an operator hands
	// a track to the workflow instead of hunting for the right folder.
	for _, mime := range []string{"audio/mpeg", "audio/wav", "video/mp4", "video/quicktime"} {
		if !mimeAllowed(mime, allow) {
			t.Errorf("%s should be allowed by default", mime)
		}
	}
	// And the allowlist is still an allowlist.
	for _, mime := range []string{"application/x-msdownload", "text/html"} {
		if mimeAllowed(mime, allow) {
			t.Errorf("%s should NOT be allowed by default", mime)
		}
	}
}
