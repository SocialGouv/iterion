package model

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/deeplink"
	"github.com/SocialGouv/iterion/pkg/store"
)

// deliverableLinks returns the preview_url_available events that
// announce a run's own product, in the order the run recorded them.
func deliverableLinks(evts []*store.Event) []*store.Event {
	var found []*store.Event
	for _, evt := range evts {
		if evt.Type != store.EventPreviewURLAvailable {
			continue
		}
		if evt.Data["kind"] == previewKindDeliverable {
			found = append(found, evt)
		}
	}
	return found
}

// publishedEvents returns the run_attachment_published pointers.
func publishedEvents(evts []*store.Event) []*store.Event {
	var found []*store.Event
	for _, evt := range evts {
		if evt.Type == store.EventRunAttachmentPublished {
			found = append(found, evt)
		}
	}
	return found
}

// declareFile writes a file a node can hand over and returns its path.
func declareFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestAPublishedDeliverableIsAnnouncedInTheRunSummary is the witness for
// #2690's surfacing half: a file the node DECLARED and the run received
// is announced once, at the address a reader can actually download it
// from, with the kind the studio lists beside the pull requests.
//
// The run id and the handle are read back from the events rather than
// spelled twice, so the assertion is "the link addresses THIS file",
// not "the link matches the string the test also built".
func TestAPublishedDeliverableIsAnnouncedInTheRunSummary(t *testing.T) {
	t.Run("published: one link, at the team-gated download address", func(t *testing.T) {
		t.Setenv(deeplink.PublicBaseEnv, "https://iterion.example")
		report := declareFile(t, "report.md", "# audit\n")

		evts, err := runDeclaringNode(t,
			"echo '[iterion] attachment="+report+" name=report'")
		if err != nil {
			t.Fatalf("the node failed while publishing a deliverable: %v", err)
		}
		published := publishedEvents(evts)
		if len(published) != 1 {
			t.Fatalf("want exactly 1 %s event, got %d", store.EventRunAttachmentPublished, len(published))
		}
		links := deliverableLinks(evts)
		if len(links) != 1 {
			t.Fatalf("want exactly 1 preview_url_available of kind %q — a run's "+
				"product is announced in the summary like a pull request — got %d: %#v",
				previewKindDeliverable, len(links), evts)
		}
		handle, _ := published[0].Data["attachment_name"].(string)
		if handle == "" {
			t.Fatalf("the published event carries no attachment_name: %#v", published[0].Data)
		}
		// Both halves of the expected address are read back from the
		// run, so an empty one cannot make the comparison vacuous.
		if published[0].RunID == "" {
			t.Fatalf("the published event carries no run id: %#v", published[0])
		}
		want := deeplink.RunAttachment("https://iterion.example", published[0].RunID, handle)
		if got := links[0].Data["url"]; got != want {
			t.Errorf("link url = %v, want %s — the announced address must be the "+
				"route that serves THIS handle, not the run's page", got, want)
		}
		if got := links[0].Data["scope"]; got != previewScopeExternal {
			t.Errorf("link scope = %v, want %q", got, previewScopeExternal)
		}
		if got := links[0].Data["source"]; got != previewSourceToolAttachment {
			t.Errorf("link source = %v, want %q — a consumer must tell an engine "+
				"announcement from a URL a tool printed", got, previewSourceToolAttachment)
		}
		if got := links[0].NodeID; got != "final_report" {
			t.Errorf("link node = %q, want final_report", got)
		}
	})

	t.Run("lost: the loss is recorded and nothing is announced", func(t *testing.T) {
		t.Setenv(deeplink.PublicBaseEnv, "https://iterion.example")
		absent := filepath.Join(t.TempDir(), "report.md")

		evts, err := runDeclaringNode(t, "echo '[iterion] attachment="+absent+"'")
		if err != nil {
			t.Fatalf("a lost deliverable without required= must not fail the node: %v", err)
		}
		if losses := lossEvents(evts); len(losses) != 1 {
			t.Fatalf("want exactly 1 %s event, got %d", store.EventRunAttachmentLost, len(losses))
		}
		if links := deliverableLinks(evts); len(links) != 0 {
			t.Errorf("a file the run never received was announced as downloadable: %#v",
				links[0].Data)
		}
	})

	t.Run("no public origin: published, and no link at all", func(t *testing.T) {
		// The documented policy: with no origin to stand on there is no
		// address a reader could follow, so the run says nothing rather
		// than publishing a root-relative path the studio would resolve
		// against the wrong server.
		t.Setenv(deeplink.PublicBaseEnv, "")
		report := declareFile(t, "report.md", "# audit\n")

		evts, err := runDeclaringNode(t, "echo '[iterion] attachment="+report+"'")
		if err != nil {
			t.Fatalf("the node failed while publishing a deliverable: %v", err)
		}
		if published := publishedEvents(evts); len(published) != 1 {
			t.Fatalf("the file must be published either way: want 1 %s, got %d",
				store.EventRunAttachmentPublished, len(published))
		}
		if links := deliverableLinks(evts); len(links) != 0 {
			t.Errorf("want no link when the deployment has no public origin, got %#v",
				links[0].Data)
		}
	})

	t.Run("unusable public origin: published, and no link at all", func(t *testing.T) {
		// A value with no scheme yields a RELATIVE link, which resolves
		// against whatever page it was pasted on (deeplink.PublicOrigin).
		t.Setenv(deeplink.PublicBaseEnv, "iterion.example")
		report := declareFile(t, "report.md", "# audit\n")

		evts, err := runDeclaringNode(t, "echo '[iterion] attachment="+report+"'")
		if err != nil {
			t.Fatalf("the node failed while publishing a deliverable: %v", err)
		}
		if published := publishedEvents(evts); len(published) != 1 {
			t.Fatalf("want 1 %s, got %d", store.EventRunAttachmentPublished, len(published))
		}
		if links := deliverableLinks(evts); len(links) != 0 {
			t.Errorf("want no link from an unusable origin, got %#v", links[0].Data)
		}
	})

	t.Run("a screenshot is a hint, not a declared deliverable", func(t *testing.T) {
		// The boundary `required=` already draws: a capture reports on
		// work that happened elsewhere and belongs to the Browser pane's
		// filmstrip, so it must not take a headline slot in the summary.
		t.Setenv(deeplink.PublicBaseEnv, "https://iterion.example")
		shot := declareFile(t, "shot.png", "\x89PNG\r\n\x1a\n")

		evts, err := runDeclaringNode(t, "echo '[iterion] preview_screenshot="+shot+"'")
		if err != nil {
			t.Fatalf("the node failed while capturing a screenshot: %v", err)
		}
		var shots int
		for _, evt := range evts {
			if evt.Type == store.EventBrowserScreenshot {
				shots++
			}
		}
		if shots != 1 {
			t.Fatalf("want exactly 1 %s event, got %d", store.EventBrowserScreenshot, shots)
		}
		if links := deliverableLinks(evts); len(links) != 0 {
			t.Errorf("a screenshot was announced as a headline deliverable: %#v",
				links[0].Data)
		}
	})
}
