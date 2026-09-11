package server

import (
	"strings"
	"testing"
)

func TestBoundProjectedChatHistory_KeepsNewestInChronologicalOrder(t *testing.T) {
	messages := []projectedChatMessage{
		{Role: "operator", Text: "one"},
		{Role: "assistant", Text: "two"},
		{Role: "operator", Text: "three"},
		{Role: "assistant", Text: "four"},
	}
	got := boundProjectedChatHistory(messages, 2, 100)
	if len(got) != 2 || got[0].Text != "three" || got[1].Text != "four" {
		t.Fatalf("bounded history=%#v", got)
	}
}

func TestBoundProjectedChatHistory_EnforcesTokenBudget(t *testing.T) {
	messages := []projectedChatMessage{
		{Role: "operator", Text: strings.Repeat("a", 90)},
		{Role: "assistant", Text: strings.Repeat("b", 90)},
		{Role: "operator", Text: "latest"},
	}
	got := boundProjectedChatHistory(messages, 8, 10)
	if len(got) != 1 || got[0].Text != "latest" {
		t.Fatalf("token-bounded history=%#v", got)
	}
}
