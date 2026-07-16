package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
	"golang.org/x/sync/errgroup"
)

// translationResult holds a single translation attempt.
type translationResult struct {
	Style string
	Text  string
	Cost  float64
}

// getAssistantText extracts text content and cost from the message stream.
func getAssistantText(
	ctx context.Context,
	msgs func(func(codexsdk.Message, error) bool),
) (string, float64, error) {
	var (
		text string
		cost float64
	)

	for msg, err := range msgs {
		if err != nil {
			return "", 0, fmt.Errorf("query: %w", err)
		}

		if m, ok := msg.(*codexsdk.AssistantMessage); ok {
			for _, block := range m.Content {
				if tb, ok := block.(*codexsdk.TextBlock); ok {
					text = tb.Text
				}
			}
		}

		if m, ok := msg.(*codexsdk.ResultMessage); ok {
			if m.Usage != nil {
				cost = float64(m.Usage.InputTokens + m.Usage.OutputTokens)
			}
		}
	}

	return text, cost, nil
}

func parallelTranslations() {
	fmt.Println("=== Parallel Translations ===")
	fmt.Println("Running 3 translation styles concurrently, then picking the best.")
	fmt.Println()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	originalText := "To be, or not to be, that is the question."

	styles := []struct {
		Name   string
		Prompt string
	}{
		{
			Name:   "Formal",
			Prompt: "You are a formal translator. Translate to French using literary, formal language.",
		},
		{
			Name:   "Casual",
			Prompt: "You are a casual translator. Translate to French using everyday, colloquial language.",
		},
		{
			Name:   "Poetic",
			Prompt: "You are a poetic translator. Translate to French preserving rhythm and beauty.",
		},
	}

	var (
		mu      sync.Mutex
		results = make([]translationResult, 0, len(styles))
	)

	g, gCtx := errgroup.WithContext(ctx)

	for _, style := range styles {
		g.Go(func() error {
			text, cost, err := getAssistantText(gCtx, codexsdk.Query(gCtx,
				codexsdk.Text(fmt.Sprintf("Translate this to French: %q", originalText)),
				codexsdk.WithSystemPrompt(style.Prompt),
			))
			if err != nil {
				return fmt.Errorf("%s translation: %w", style.Name, err)
			}

			mu.Lock()

			results = append(results, translationResult{
				Style: style.Name,
				Text:  text,
				Cost:  cost,
			})
			mu.Unlock()

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		fmt.Printf("Error: %v\n", err)

		return
	}

	// Display all translations
	var (
		totalCost    float64
		descriptions []string
	)

	for _, r := range results {
		fmt.Printf("[%s] %s\n", r.Style, r.Text)
		totalCost += r.Cost
		descriptions = append(descriptions,
			fmt.Sprintf("- %s: %s", r.Style, r.Text))
	}

	fmt.Printf("\nTranslation cost: $%.4f\n", totalCost)

	// Use a judge query to pick the best
	fmt.Println("\n--- Judge Evaluation ---")

	judgePrompt := fmt.Sprintf(
		"Original: %q\n\nTranslations:\n%s\n\n"+
			"Which translation is best and why? Be concise (2-3 sentences).",
		originalText, strings.Join(descriptions, "\n"),
	)

	judgeText, judgeCost, err := getAssistantText(ctx, codexsdk.Query(ctx,
		codexsdk.Text(judgePrompt),
		codexsdk.WithSystemPrompt(
			"You are a French language expert. Evaluate translations for accuracy and style.",
		),
	))
	if err != nil {
		fmt.Printf("Judge error: %v\n", err)

		return
	}

	fmt.Printf("Judge: %s\n", judgeText)
	fmt.Printf("\nTotal cost (translations + judge): $%.4f\n", totalCost+judgeCost)
	fmt.Println()
}

func main() {
	fmt.Println("Parallel Queries Examples")
	fmt.Println()
	fmt.Println("Demonstrates running multiple Query() calls concurrently with errgroup.")
	fmt.Println()

	parallelTranslations()
}
