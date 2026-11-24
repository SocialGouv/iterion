package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testNotebook = `{
  "cells": [
    {"cell_type": "code", "source": ["print('hello')\n"], "metadata": {}, "outputs": [], "execution_count": null, "id": "cell-0"},
    {"cell_type": "markdown", "source": ["# Title\n"], "metadata": {}, "id": "cell-1"}
  ],
  "metadata": {"kernelspec": {"language": "python"}},
  "nbformat": 4,
  "nbformat_minor": 5
}`

func writeTestNotebook(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "test.ipynb")
	if err := os.WriteFile(p, []byte(testNotebook), 0644); err != nil {
		t.Fatalf("failed to write test notebook: %v", err)
	}
	return p
}

func readCells(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read notebook: %v", err)
	}
	var nb map[string]json.RawMessage
	if err := json.Unmarshal(data, &nb); err != nil {
		t.Fatalf("cannot parse notebook: %v", err)
	}
	var cells []map[string]any
	if err := json.Unmarshal(nb["cells"], &cells); err != nil {
		t.Fatalf("cannot parse cells: %v", err)
	}
	return cells
}

func TestNotebookEdit(t *testing.T) {
	t.Run("replace cell 0 source", func(t *testing.T) {
		p := writeTestNotebook(t)
		out, err := ExecuteNotebookEdit(map[string]any{
			"notebook_path": p,
			"cell_id":       "cell-0",
			"new_source":    "print('world')",
			"edit_mode":     "replace",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(out, "replace") {
			t.Fatalf("expected output to mention replace, got %s", out)
		}
		cells := readCells(t, p)
		src, _ := json.Marshal(cells[0]["source"])
		if !strings.Contains(string(src), "print('world')") {
			t.Fatalf("cell 0 source not updated: %s", src)
		}
	})

	t.Run("insert cell after cell 0", func(t *testing.T) {
		p := writeTestNotebook(t)
		out, err := ExecuteNotebookEdit(map[string]any{
			"notebook_path": p,
			"cell_id":       "cell-0",
			"new_source":    "x = 1",
			"edit_mode":     "insert",
			"cell_type":     "code",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(out, "insert") {
			t.Fatalf("expected output to mention insert, got %s", out)
		}
		cells := readCells(t, p)
		if len(cells) != 3 {
			t.Fatalf("expected 3 cells after insert, got %d", len(cells))
		}
	})

	t.Run("delete cell 0", func(t *testing.T) {
		p := writeTestNotebook(t)
		out, err := ExecuteNotebookEdit(map[string]any{
			"notebook_path": p,
			"cell_id":       "cell-0",
			"edit_mode":     "delete",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(out, "delete") {
			t.Fatalf("expected output to mention delete, got %s", out)
		}
		cells := readCells(t, p)
		if len(cells) != 1 {
			t.Fatalf("expected 1 cell after delete, got %d", len(cells))
		}
	})

	t.Run("missing notebook_path", func(t *testing.T) {
		_, err := ExecuteNotebookEdit(map[string]any{})
		if err == nil || !strings.Contains(err.Error(), "'notebook_path' is required") {
			t.Fatalf("expected notebook_path required error, got %v", err)
		}
	})

	t.Run("cell not found", func(t *testing.T) {
		p := writeTestNotebook(t)
		_, err := ExecuteNotebookEdit(map[string]any{
			"notebook_path": p,
			"cell_id":       "nonexistent",
			"new_source":    "x",
			"edit_mode":     "replace",
		})
		if err == nil || !strings.Contains(err.Error(), "cell not found") {
			t.Fatalf("expected cell not found error, got %v", err)
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "bad.ipynb")
		os.WriteFile(p, []byte("{not valid json"), 0644)
		_, err := ExecuteNotebookEdit(map[string]any{
			"notebook_path": p,
			"cell_id":       "0",
			"edit_mode":     "replace",
			"new_source":    "x",
		})
		if err == nil || !strings.Contains(err.Error(), "invalid notebook JSON") {
			t.Fatalf("expected invalid JSON error, got %v", err)
		}
	})
}
