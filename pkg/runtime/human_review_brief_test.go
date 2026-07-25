package runtime

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestExtractHumanReviewBriefValidatesAndStampsPoints(t *testing.T) {
	questions := map[string]any{
		humanReviewPointsKey: []any{
			"  Vérifiez   que la composition reste lisible.  ",
			"Confirmez que les éléments importants sont immédiatement visibles.",
		},
	}

	got := extractHumanReviewBrief(questions)
	if got == nil {
		t.Fatal("extractHumanReviewBrief returned nil")
	}
	if got.Version != store.HumanReviewBriefVersion || got.Source != store.HumanReviewBriefSourceAI {
		t.Fatalf("runtime metadata = %+v", got)
	}
	want := []string{
		"Vérifiez que la composition reste lisible.",
		"Confirmez que les éléments importants sont immédiatement visibles.",
	}
	if !reflect.DeepEqual(got.Points, want) {
		t.Errorf("points = %#v, want %#v", got.Points, want)
	}
	if _, leaked := questions[humanReviewPointsKey]; leaked {
		t.Error("ai_review_points was not consumed")
	}
}

func TestExtractHumanReviewBriefRejectsUnsafeOrInvalidPayloads(t *testing.T) {
	valid240 := strings.Repeat("é", maxHumanReviewPointChars)
	tests := map[string]any{
		"not an array":   "Check the result",
		"empty":          []any{},
		"too many":       []any{"one", "two", "three", "four"},
		"non string":     []any{"one", 2},
		"blank":          []any{" \n\t "},
		"point too long": []any{valid240 + "x"},
		"total too long": []any{strings.Repeat("a", 201), strings.Repeat("b", 201), strings.Repeat("c", 201)},
		"url":            []any{"Ouvrez https://example.test/review."},
		"path":           []any{"Comparez renders/final.png avec la maquette."},
		"bare file":      []any{"Consultez vertical-plan.json."},
		"uuid":           []any{"Validez 0599fa02-996d-42c7-ba17-8f395c83f6e7."},
		"hash":           []any{"Comparez avec deadbeef avant validation."},
	}

	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			questions := map[string]any{humanReviewPointsKey: raw}
			if got := extractHumanReviewBrief(questions); got != nil {
				t.Fatalf("brief = %+v, want nil", got)
			}
			if _, leaked := questions[humanReviewPointsKey]; leaked {
				t.Error("invalid ai_review_points was not consumed")
			}
		})
	}

	boundary := map[string]any{
		humanReviewPointsKey: []string{
			valid240,
			strings.Repeat("a", maxHumanReviewPointChars),
			strings.Repeat("b", maxHumanReviewBriefChars-2*maxHumanReviewPointChars),
		},
	}
	if got := extractHumanReviewBrief(boundary); got == nil {
		t.Fatal("exact per-point and total limits should be accepted")
	}
}

func TestDoPausePersistsHumanReviewBriefAcrossReadModels(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	if _, err := s.CreateRun(ctx, "brief-pause", "review_test", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	eng := New(minimalReviewWorkflow(), s, newStubExecutor())
	rs := eng.newRunState("brief-pause", nil)
	rs.ctx = ctx
	questions := map[string]any{
		"approved": "Do you approve?",
		humanReviewPointsKey: []any{
			"Check that the main choice is immediately understandable.",
			"Confirm that no required content is missing.",
		},
	}

	if err := eng.doPause(rs, "gate", questions, map[string]any{"instructions": "Review it."}, pauseInfo{}); err != nil {
		t.Fatalf("doPause: %v", err)
	}
	if _, leaked := questions[humanReviewPointsKey]; leaked {
		t.Error("ai_review_points remained in caller questions")
	}

	run, err := s.LoadRun(ctx, "brief-pause")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	brief := run.Checkpoint.InteractionReviewBrief
	if brief == nil || brief.Version != 1 || brief.Source != "ai" || len(brief.Points) != 2 {
		t.Fatalf("checkpoint review brief = %+v", brief)
	}
	if _, leaked := run.Checkpoint.InteractionQuestions[humanReviewPointsKey]; leaked {
		t.Error("ai_review_points leaked into checkpoint questions")
	}

	interaction, err := s.LoadInteraction(ctx, "brief-pause", run.Checkpoint.InteractionID)
	if err != nil {
		t.Fatalf("LoadInteraction: %v", err)
	}
	if interaction.ReviewBrief == nil || !reflect.DeepEqual(interaction.ReviewBrief.Points, brief.Points) {
		t.Fatalf("interaction review brief = %+v, checkpoint = %+v", interaction.ReviewBrief, brief)
	}
	if _, leaked := interaction.Questions[humanReviewPointsKey]; leaked {
		t.Error("ai_review_points leaked into interaction questions")
	}

	events, err := s.LoadEvents(ctx, "brief-pause")
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	found := false
	for _, event := range events {
		if event.Type != store.EventHumanInputRequested {
			continue
		}
		found = true
		wire, ok := event.Data["review_brief"].(map[string]any)
		if !ok {
			t.Fatalf("event review_brief = %#v", event.Data["review_brief"])
		}
		if wire["source"] != "ai" {
			t.Errorf("event review_brief source = %#v", wire["source"])
		}
		eventQuestions, _ := event.Data["questions"].(map[string]any)
		if _, leaked := eventQuestions[humanReviewPointsKey]; leaked {
			t.Error("ai_review_points leaked into event questions")
		}
	}
	if !found {
		t.Fatal("human_input_requested event not found")
	}
}
