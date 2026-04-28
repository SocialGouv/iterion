package tools

import (
	"github.com/SocialGouv/claw-code-go/internal/api"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func NotebookEditTool() api.Tool {
	return api.Tool{
		Name:        "notebook_edit",
		Description: "Edit Jupyter notebook cells. Supports replace, insert, and delete operations on .ipynb files.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"notebook_path": {Type: "string", Description: "Path to the .ipynb file."},
				"cell_id":       {Type: "string", Description: "Target cell ID or index (0-based)."},
				"new_source":    {Type: "string", Description: "New cell source content."},
				"cell_type":     {Type: "string", Description: "Cell type: code or markdown."},
				"edit_mode":     {Type: "string", Description: "Operation: replace, insert, or delete."},
			},
			Required: []string{"notebook_path"},
		},
	}
}

func ExecuteNotebookEdit(input map[string]any) (string, error) {
	nbPath, ok := input["notebook_path"].(string)
	if !ok || nbPath == "" {
		return "", fmt.Errorf("notebook_edit: 'notebook_path' is required")
	}

	data, err := os.ReadFile(nbPath)
	if err != nil {
		return "", fmt.Errorf("notebook_edit: cannot read %s: %v", nbPath, err)
	}

	// Parse notebook preserving raw cells via json.RawMessage
	var notebook map[string]json.RawMessage
	if err := json.Unmarshal(data, &notebook); err != nil {
		return "", fmt.Errorf("notebook_edit: invalid notebook JSON: %v", err)
	}

	rawCells, ok := notebook["cells"]
	if !ok {
		return "", fmt.Errorf("notebook_edit: notebook has no 'cells' field")
	}

	var cells []json.RawMessage
	if err := json.Unmarshal(rawCells, &cells); err != nil {
		return "", fmt.Errorf("notebook_edit: cannot parse cells: %v", err)
	}

	editMode := "replace"
	if m, ok := input["edit_mode"].(string); ok && m != "" {
		editMode = strings.ToLower(m)
	}

	cellType := "code"
	if ct, ok := input["cell_type"].(string); ok && ct != "" {
		cellType = strings.ToLower(ct)
	}

	newSource, _ := input["new_source"].(string)
	cellID, _ := input["cell_id"].(string)

	// Find target cell index
	targetIdx := -1
	if cellID != "" {
		// Try numeric index first
		if idx, err := strconv.Atoi(cellID); err == nil && idx >= 0 && idx < len(cells) {
			targetIdx = idx
		} else {
			// Try matching by cell ID in metadata
			for i, raw := range cells {
				var cell map[string]json.RawMessage
				if err := json.Unmarshal(raw, &cell); err == nil {
					if meta, ok := cell["metadata"]; ok {
						var metaMap map[string]json.RawMessage
						if err := json.Unmarshal(meta, &metaMap); err == nil {
							if idRaw, ok := metaMap["id"]; ok {
								var id string
								if err := json.Unmarshal(idRaw, &id); err == nil && id == cellID {
									targetIdx = i
									break
								}
							}
						}
					}
					// Also check top-level id field
					if idRaw, ok := cell["id"]; ok {
						var id string
						if err := json.Unmarshal(idRaw, &id); err == nil && id == cellID {
							targetIdx = i
							break
						}
					}
				}
			}
		}
	}

	var resultCellID string

	switch editMode {
	case "replace":
		if targetIdx < 0 {
			return "", fmt.Errorf("notebook_edit: cell not found: %s", cellID)
		}
		if newSource == "" {
			return "", fmt.Errorf("notebook_edit: 'new_source' is required for replace mode")
		}

		var cell map[string]any
		if err := json.Unmarshal(cells[targetIdx], &cell); err != nil {
			return "", fmt.Errorf("notebook_edit: cannot parse target cell: %v", err)
		}

		// Split source into lines array (Jupyter format)
		cell["source"] = splitNotebookSource(newSource)
		cell["cell_type"] = cellType

		if cellType == "code" {
			if _, ok := cell["outputs"]; !ok {
				cell["outputs"] = []any{}
			}
			if _, ok := cell["execution_count"]; !ok {
				cell["execution_count"] = nil
			}
		} else {
			delete(cell, "outputs")
			delete(cell, "execution_count")
		}

		updatedCell, _ := json.Marshal(cell)
		cells[targetIdx] = updatedCell
		resultCellID = cellID

	case "insert":
		if newSource == "" {
			return "", fmt.Errorf("notebook_edit: 'new_source' is required for insert mode")
		}

		newCell := map[string]any{
			"cell_type": cellType,
			"source":    splitNotebookSource(newSource),
			"metadata":  map[string]any{},
		}
		if cellType == "code" {
			newCell["outputs"] = []any{}
			newCell["execution_count"] = nil
		}

		newCellJSON, _ := json.Marshal(newCell)

		insertIdx := len(cells) // append by default
		if targetIdx >= 0 {
			insertIdx = targetIdx + 1 // insert after target
		}

		// Insert at position
		cells = append(cells, nil)
		copy(cells[insertIdx+1:], cells[insertIdx:])
		cells[insertIdx] = newCellJSON
		resultCellID = fmt.Sprintf("new_cell_%d", insertIdx)

	case "delete":
		if targetIdx < 0 {
			return "", fmt.Errorf("notebook_edit: cell not found for delete: %s", cellID)
		}
		resultCellID = cellID
		cells = append(cells[:targetIdx], cells[targetIdx+1:]...)

	default:
		return "", fmt.Errorf("notebook_edit: unsupported edit_mode %q (supported: replace, insert, delete)", editMode)
	}

	// Re-serialize cells into notebook
	cellsJSON, _ := json.Marshal(cells)
	notebook["cells"] = cellsJSON

	// Reconstruct the notebook JSON preserving key order via ordered output
	outJSON, err := json.MarshalIndent(notebook, "", " ")
	if err != nil {
		return "", fmt.Errorf("notebook_edit: failed to serialize notebook: %v", err)
	}

	if err := os.WriteFile(nbPath, outJSON, 0644); err != nil {
		return "", fmt.Errorf("notebook_edit: failed to write %s: %v", nbPath, err)
	}

	// Detect language from notebook metadata
	language := "python"
	if metaRaw, ok := notebook["metadata"]; ok {
		var meta map[string]json.RawMessage
		if err := json.Unmarshal(metaRaw, &meta); err == nil {
			if ksRaw, ok := meta["kernelspec"]; ok {
				var ks map[string]string
				if err := json.Unmarshal(ksRaw, &ks); err == nil {
					if l, ok := ks["language"]; ok && l != "" {
						language = l
					}
				}
			}
		}
	}

	result := map[string]any{
		"cell_id":   resultCellID,
		"cell_type": cellType,
		"edit_mode": editMode,
		"language":  language,
	}
	if newSource != "" {
		result["new_source"] = newSource
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), nil
}

// splitNotebookSource splits source text into lines with trailing newlines
// as Jupyter expects in the source array.
func splitNotebookSource(s string) []string {
	if s == "" {
		return []string{}
	}
	lines := strings.Split(s, "\n")
	result := make([]string, len(lines))
	for i, line := range lines {
		if i < len(lines)-1 {
			result[i] = line + "\n"
		} else {
			result[i] = line
		}
	}
	return result
}
