package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zendev-sh/goai"
)

const maxGrepResults = 1000

var grepSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"pattern": {
			"type": "string",
			"description": "Regular expression pattern to search for"
		},
		"path": {
			"type": "string",
			"description": "Directory or file to search in"
		},
		"glob": {
			"type": "string",
			"description": "File glob filter (e.g., '*.go', '*.ts'). Only used when path is a directory."
		}
	},
	"required": ["pattern", "path"]
}`)

// Grep returns a goai.Tool that searches for a regex pattern in files.
// Paths are resolved relative to workDir and must not escape it.
func Grep(workDir string) goai.Tool {
	return goai.Tool{
		Name:        "grep",
		Description: "Search for a regex pattern in files. Returns matching lines in file:line:content format. Skips .git, node_modules, and vendor directories by default when searching a directory.",
		InputSchema: grepSchema,
		Execute: func(_ context.Context, input json.RawMessage) (string, error) {
			var params struct {
				Pattern string `json:"pattern"`
				Path    string `json:"path"`
				Glob    string `json:"glob"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("grep: parse input: %w", err)
			}
			if params.Pattern == "" {
				return "", fmt.Errorf("grep: 'pattern' is required")
			}
			if params.Path == "" {
				return "", fmt.Errorf("grep: 'path' is required")
			}

			resolved, err := safePath(workDir, params.Path)
			if err != nil {
				return "", fmt.Errorf("grep: %w", err)
			}

			re, err := regexp.Compile(params.Pattern)
			if err != nil {
				return "", fmt.Errorf("grep: invalid pattern: %w", err)
			}

			var results []string

			info, err := os.Stat(resolved)
			if err != nil {
				return "", fmt.Errorf("grep: stat path: %w", err)
			}

			if info.IsDir() {
				err = filepath.Walk(resolved, func(path string, fi os.FileInfo, err error) error {
					if err != nil {
						return nil
					}
					if fi.IsDir() {
						name := fi.Name()
						if (name == ".git" || name == "node_modules" || name == "vendor") && path != resolved {
							return filepath.SkipDir
						}
						return nil
					}
					if params.Glob != "" {
						matched, _ := filepath.Match(params.Glob, filepath.Base(path))
						if !matched {
							return nil
						}
					}
					fileResults := grepFile(re, path)
					results = append(results, fileResults...)
					if len(results) >= maxGrepResults {
						return filepath.SkipAll
					}
					return nil
				})
				if err != nil && err != filepath.SkipAll {
					return "", fmt.Errorf("grep walk: %w", err)
				}
			} else {
				results = grepFile(re, resolved)
			}

			if len(results) == 0 {
				return fmt.Sprintf("No matches found for pattern: %s", params.Pattern), nil
			}

			output := strings.Join(results, "\n")
			if len(results) >= maxGrepResults {
				output += fmt.Sprintf("\n... [truncated at %d results]", maxGrepResults)
			}

			return output, nil
		},
	}
}

func grepFile(re *regexp.Regexp, path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var results []string
	scanner := bufio.NewScanner(f)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		if re.MatchString(line) {
			results = append(results, fmt.Sprintf("%s:%d:%s", path, lineNum, line))
		}
	}

	return results
}
