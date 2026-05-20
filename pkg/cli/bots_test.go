package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBotsList_ParsesFrontmatter(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.bot"), `## ---
## name: alpha
## description: First bot
## triggers: [refactor]
## capabilities: [board.read]
## ---

## Some other comment.
agent x:
  model: "test"
`)
	var buf bytes.Buffer
	err := BotsList(BotsListOptions{Paths: []string{dir}, Format: "json"}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	var entries []BotEntry
	if err := json.Unmarshal(buf.Bytes(), &entries); err != nil {
		t.Fatalf("decode: %v\n%s", err, buf.String())
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Name != "alpha" {
		t.Errorf("Name = %q", e.Name)
	}
	if e.Description != "First bot" {
		t.Errorf("Description = %q", e.Description)
	}
	if strings.Join(e.Triggers, ",") != "refactor" {
		t.Errorf("Triggers = %v", e.Triggers)
	}
	if strings.Join(e.Capabilities, ",") != "board.read" {
		t.Errorf("Capabilities = %v", e.Capabilities)
	}
}

func TestBotsList_FallbackToFilenameAndLeadingComment(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "feature_dev.bot"), `## ─────────────────────────────────────────────────────────────
## Autonomous feature developer.
## Plans, implements, simplifies.
## ─────────────────────────────────────────────────────────────

agent x:
  model: "test"
`)
	var buf bytes.Buffer
	if err := BotsList(BotsListOptions{Paths: []string{dir}, Format: "json"}, &buf); err != nil {
		t.Fatal(err)
	}
	var entries []BotEntry
	_ = json.Unmarshal(buf.Bytes(), &entries)
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	if entries[0].Name != "feature_dev" {
		t.Fatalf("Name = %q", entries[0].Name)
	}
	if !strings.Contains(entries[0].Description, "Autonomous feature developer") {
		t.Fatalf("Description = %q", entries[0].Description)
	}
}

func TestBotsList_Bundle(t *testing.T) {
	dir := t.TempDir()
	bundle := filepath.Join(dir, "my-bundle")
	writeFile(t, filepath.Join(bundle, "manifest.yaml"), `name: my-bundle
description: |
  A great bot bundle.
triggers: [pipe]
`)
	writeFile(t, filepath.Join(bundle, "main.bot"), `agent x:
  model: "test"
`)
	var buf bytes.Buffer
	if err := BotsList(BotsListOptions{Paths: []string{dir}, Format: "json"}, &buf); err != nil {
		t.Fatal(err)
	}
	var entries []BotEntry
	_ = json.Unmarshal(buf.Bytes(), &entries)
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	if entries[0].Name != "my-bundle" {
		t.Fatalf("Name = %q", entries[0].Name)
	}
	if !strings.Contains(entries[0].Description, "A great bot bundle") {
		t.Fatalf("Description = %q", entries[0].Description)
	}
	if entries[0].Triggers[0] != "pipe" {
		t.Fatalf("Triggers = %v", entries[0].Triggers)
	}
}

func TestBotsList_FormatMarkdown(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "x.bot"), `## ---
## name: x
## description: X
## ---
`)
	var buf bytes.Buffer
	if err := BotsList(BotsListOptions{Paths: []string{dir}, Format: "markdown"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "## x") {
		t.Fatalf("markdown output missing bot header:\n%s", buf.String())
	}
}

func TestBotsList_FormatSkill(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "x.bot"), `## ---
## name: x
## description: X
## triggers: [foo]
## ---
`)
	var buf bytes.Buffer
	if err := BotsList(BotsListOptions{Paths: []string{dir}, Format: "skill"}, &buf); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"name: iterion-bot-catalog",
		"| `x` | X | foo |",
		"Assignment heuristics",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("skill output missing %q:\n%s", want, buf.String())
		}
	}
}
