package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

// Person represents a simple structured output schema.
type Person struct {
	Name    string   `json:"name"`
	Age     int      `json:"age"`
	Hobbies []string `json:"hobbies"`
}

// BookReview represents a more complex nested structured output.
type BookReview struct {
	Title  string `json:"title"`
	Author string `json:"author"`
	Rating int    `json:"rating"`
	Review struct {
		Summary  string   `json:"summary"`
		Pros     []string `json:"pros"`
		Cons     []string `json:"cons"`
		Audience string   `json:"audience"`
	} `json:"review"`
}

func marshalSchema(schema map[string]any) (string, error) {
	data, err := json.Marshal(schema)
	if err != nil {
		return "", err
	}

	return string(data), nil
}

// getStructuredOutput runs a query and returns structured JSON.
func getStructuredOutput(ctx context.Context, prompt string, schema string, systemPrompt string) (json.RawMessage, error) {
	var lastAssistantText string

	for msg, err := range codexsdk.Query(ctx, codexsdk.Text(prompt),
		codexsdk.WithOutputSchema(schema),
		codexsdk.WithSystemPrompt(systemPrompt),
	) {
		if err != nil {
			return nil, fmt.Errorf("query: %w", err)
		}

		switch m := msg.(type) {
		case *codexsdk.AssistantMessage:
			for _, block := range m.Content {
				if tb, ok := block.(*codexsdk.TextBlock); ok {
					lastAssistantText += tb.Text
				}
			}

		case *codexsdk.ResultMessage:
			if m.Usage != nil {
				fmt.Printf("Tokens: %d in / %d out\n", m.Usage.InputTokens, m.Usage.OutputTokens)
			}

			if data, ok := decodeStructuredPayload(m.StructuredOutput, m.Result); ok {
				return data, nil
			}
		}
	}

	if data, ok := decodeStructuredPayload(lastAssistantText, nil); ok {
		return data, nil
	}

	return nil, fmt.Errorf("no structured output received")
}

func simpleStructuredOutput() {
	fmt.Println("=== Simple Structured Output ===")
	fmt.Println("Using WithOutputSchema() to get a JSON Person object.")
	fmt.Println()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"name":    map[string]any{"type": "string"},
			"age":     map[string]any{"type": "integer"},
			"hobbies": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required": []string{"name", "age", "hobbies"},
	}

	schemaJSON, err := marshalSchema(schema)
	if err != nil {
		fmt.Printf("Error marshaling schema: %v\n", err)

		return
	}

	output, err := getStructuredOutput(
		ctx,
		"Invent a fictional person with a name, age, and exactly 3 hobbies.",
		schemaJSON,
		"You are a creative writer. Respond only with valid JSON matching the schema.",
	)
	if err != nil {
		fmt.Printf("Error: %v\n", err)

		return
	}

	var person Person
	if err := json.Unmarshal(output, &person); err != nil {
		fmt.Printf("Failed to parse JSON: %v\n", err)
		fmt.Printf("Raw output: %s\n", string(output))

		return
	}

	fmt.Printf("Name:    %s\n", person.Name)
	fmt.Printf("Age:     %d\n", person.Age)
	fmt.Printf("Hobbies: %v\n", person.Hobbies)
	fmt.Println()
}

func nestedStructuredOutput() {
	fmt.Println("=== Nested Structured Output ===")
	fmt.Println("Using WithOutputSchema() with a complex nested schema.")
	fmt.Println()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"title":  map[string]any{"type": "string", "enum": []string{"Nineteen Eighty-Four"}},
			"author": map[string]any{"type": "string"},
			"rating": map[string]any{"type": "integer", "minimum": 1, "maximum": 5},
			"review": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"summary":  map[string]any{"type": "string"},
					"pros":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"cons":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"audience": map[string]any{"type": "string"},
				},
				"required": []string{"summary", "pros", "cons", "audience"},
			},
		},
		"required": []string{"title", "author", "rating", "review"},
	}

	schemaJSON, err := marshalSchema(schema)
	if err != nil {
		fmt.Printf("Error marshaling schema: %v\n", err)

		return
	}

	output, err := getStructuredOutput(
		ctx,
		"Write a brief review of George Orwell's novel \"Nineteen Eighty-Four\". Return that exact title as a JSON string, along with the author, a rating from 1-5, and a review with a short summary, 2 pros, 2 cons, and target audience.",
		schemaJSON,
		"You are a book critic. Respond only with valid JSON matching the schema.",
	)
	if err != nil {
		fmt.Printf("Error: %v\n", err)

		return
	}

	var review BookReview
	if err := json.Unmarshal(output, &review); err != nil {
		fmt.Printf("Failed to parse JSON: %v\n", err)
		fmt.Printf("Raw output: %s\n", string(output))

		return
	}

	fmt.Printf("Title:    %s\n", review.Title)
	fmt.Printf("Author:   %s\n", review.Author)
	fmt.Printf("Rating:   %d/5\n", review.Rating)
	fmt.Printf("Summary:  %s\n", review.Review.Summary)
	fmt.Printf("Pros:     %v\n", review.Review.Pros)
	fmt.Printf("Cons:     %v\n", review.Review.Cons)
	fmt.Printf("Audience: %s\n", review.Review.Audience)
	fmt.Println()
}

func persistentStructuredOutput() {
	fmt.Println("=== Persistent Structured Output ===")
	fmt.Println("Using NewClient() + WithOutputFormat() for session-scoped structured output.")
	fmt.Println()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client := codexsdk.NewClient()
	defer client.Close()

	if err := client.Start(ctx,
		codexsdk.WithPermissionMode("bypassPermissions"),
		codexsdk.WithSystemPrompt("Respond only with valid JSON matching the active schema."),
		codexsdk.WithOutputFormat(map[string]any{
			"type": "json_schema",
			"schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"answer": map[string]any{"type": "string", "enum": []string{"four"}},
				},
				"required":             []string{"answer"},
				"additionalProperties": false,
			},
		}),
	); err != nil {
		fmt.Printf("Error starting client: %v\n", err)

		return
	}

	err := client.Query(ctx, codexsdk.Text("Return a JSON object with an answer field containing the string \"four\" for 2+2."))
	if err != nil {
		fmt.Printf("Error sending query: %v\n", err)

		return
	}

	for msg, recvErr := range client.ReceiveResponse(ctx) {
		if recvErr != nil {
			fmt.Printf("Error receiving response: %v\n", recvErr)

			return
		}

		if result, ok := msg.(*codexsdk.ResultMessage); ok {
			if data, ok := decodeStructuredPayload(result.StructuredOutput, result.Result); ok {
				formatted, marshalErr := json.MarshalIndent(data, "", "  ")
				if marshalErr != nil {
					fmt.Printf("Failed to format structured output: %v\n", marshalErr)
				} else {
					fmt.Printf("Structured output:\n%s\n\n", string(formatted))
				}

				return
			}
		}
	}
}

func main() {
	fmt.Println("Structured Output Examples")
	fmt.Println()
	fmt.Println("Demonstrates structured JSON responses using output schema constraints.")
	fmt.Println()

	simpleStructuredOutput()
	nestedStructuredOutput()
	persistentStructuredOutput()
}

func decodeStructuredPayload(primary any, fallback *string) (json.RawMessage, bool) {
	candidates := []any{primary}
	if fallback != nil {
		candidates = append(candidates, *fallback)
	}

	for _, candidate := range candidates {
		normalized, ok := normalizeJSONCandidate(candidate)
		if !ok {
			continue
		}

		data, err := json.Marshal(normalized)
		if err == nil {
			return data, true
		}
	}

	return nil, false
}

func normalizeJSONCandidate(candidate any) (any, bool) {
	switch v := candidate.(type) {
	case nil:
		return nil, false
	case json.RawMessage:
		return normalizeJSONString(string(v))
	case []byte:
		return normalizeJSONString(string(v))
	case string:
		return normalizeJSONString(v)
	default:
		return canonicalizeJSONValue(v), true
	}
}

func normalizeJSONString(raw string) (any, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || !json.Valid([]byte(raw)) {
		return nil, false
	}

	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, false
	}

	return canonicalizeJSONValue(parsed), true
}

func canonicalizeJSONValue(value any) any {
	switch v := value.(type) {
	case string:
		if parsed, ok := normalizeJSONString(v); ok {
			return parsed
		}

		return v
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = canonicalizeJSONValue(item)
		}

		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = canonicalizeJSONValue(item)
		}

		if len(out) == 1 {
			for key, item := range out {
				if nested, ok := item.(map[string]any); ok && len(nested) == 1 {
					if inner, ok := nested[key]; ok {
						out[key] = inner
					}
				}
			}
		}

		return out
	default:
		return v
	}
}
