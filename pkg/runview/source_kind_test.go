package runview

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// deriveSourceKind is the contract the studio mirrors (RunSourceKind in
// studio/src/api/runs/types.ts): every value it can emit must have a
// matching case there, or the run renders as "Manual".
func TestDeriveSourceKind(t *testing.T) {
	cases := []struct {
		name string
		run  store.Run
		want string
	}{
		{"plain launch", store.Run{}, "manual"},
		{"schedule", store.Run{Source: &store.RunSource{Kind: store.RunSourceKindSchedule, ScheduleID: "s1"}}, "schedule"},
		{"dispatcher", store.Run{Source: &store.RunSource{Kind: store.RunSourceKindDispatcher, IssueID: "native:1"}}, "dispatcher"},
		{"studio chat", store.Run{Source: &store.RunSource{Kind: store.RunSourceKindStudioChat, ClientID: "browser", ConversationID: "tab"}}, "studio_chat"},
		{"webhook owner", store.Run{OwnerID: "webhook:gitlab"}, "webhook"},
		{"fork", store.Run{ForkedFrom: "run-parent"}, "fork"},
		{"shard", store.Run{ParentRunID: "run-parent"}, "shard"},
		// Typed provenance outranks the structural fallbacks.
		{"schedule over shard", store.Run{ParentRunID: "p", Source: &store.RunSource{Kind: store.RunSourceKindSchedule}}, "schedule"},
		// An empty Source struct is not provenance.
		{"empty source", store.Run{Source: &store.RunSource{}}, "manual"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.run
			if got := deriveSourceKind(&r); got != tc.want {
				t.Fatalf("deriveSourceKind = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSummarizeRunCarriesAnIndependentSource(t *testing.T) {
	r := &store.Run{
		ID:           "chat-run",
		WorkflowName: "copilot",
		Source: &store.RunSource{
			Kind:           store.RunSourceKindStudioChat,
			ClientID:       "browser",
			ConversationID: "tab",
		},
	}
	summary := summarizeRun(r, false)
	if summary.Source == nil || summary.Source.ClientID != "browser" || summary.Source.ConversationID != "tab" {
		t.Fatalf("summary source = %+v", summary.Source)
	}
	summary.Source.ClientID = "changed"
	if r.Source.ClientID != "browser" {
		t.Fatal("summary source aliases persisted run source")
	}
}
