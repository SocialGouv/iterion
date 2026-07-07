package tools

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/SocialGouv/claw-code-go/internal/api"
)

// semantic_search ranks code chunks by vocabulary relevance to a natural-
// language query: identifier-aware tokenization (camelCase / snake_case
// split) + BM25 over overlapping line windows, built per call — no index on
// disk, no network, no credentials. It finds code by what it is ABOUT when
// the exact strings are unknown; grep stays better for exact symbols. An
// embeddings-backed implementation can later slot in behind the same tool.

const (
	semSearchChunkLines  = 40
	semSearchChunkStride = 30
	semSearchMaxFileSize = 512 * 1024
	semSearchMaxFiles    = 5000
	semSearchMaxBytes    = 64 * 1024 * 1024
	semSearchDefaultTopN = 8
	semSearchMaxTopN     = 25
)

// semSearchSkipDirs never contain user code worth ranking.
var semSearchSkipDirs = map[string]struct{}{
	".git": {}, "node_modules": {}, "vendor": {}, ".devbox": {}, "dist": {},
	"build": {}, "target": {}, ".next": {}, ".cache": {}, "__pycache__": {},
}

// SemanticSearchTool returns the tool definition for relevance-ranked code search.
func SemanticSearchTool() api.Tool {
	return api.Tool{
		Name: "semantic_search",
		Description: "Search code by MEANING-ADJACENT vocabulary rather than exact text: the query is matched " +
			"against identifier-split tokens (camelCase/snake_case are decomposed) and chunks are ranked by " +
			"relevance (BM25). Use it for \"where is X handled?\" questions when you don't know the exact " +
			"symbol or wording — e.g. \"http retry backoff\", \"permission prompt caching\". " +
			"Use grep for exact symbols/strings and glob for file names. Returns the top-ranked file:line " +
			"spans with a short excerpt; read the winning files before acting.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"query":       {Type: "string", Description: "Natural-language description of the code you are looking for."},
				"path":        {Type: "string", Description: "Directory to search (default: the workspace root)."},
				"glob":        {Type: "string", Description: "Optional file-name filter (e.g. '*.go', '*.ts')."},
				"max_results": {Type: "integer", Description: "Number of chunks to return (default 8, max 25)."},
			},
			Required: []string{"query"},
		},
	}
}

type semChunk struct {
	path      string
	startLine int
	endLine   int
	tokens    map[string]int
	length    int
	firstLine string
	score     float64
}

// ExecuteSemanticSearch runs the ranked search rooted at input path (or workDir).
func ExecuteSemanticSearch(input map[string]any, workDir string) (string, error) {
	query := strings.TrimSpace(stringVal(input, "query"))
	if query == "" {
		return "", fmt.Errorf("semantic_search: 'query' is required")
	}
	queryTokens := dedupeTokens(semTokenize(query))
	if len(queryTokens) == 0 {
		return "", fmt.Errorf("semantic_search: query %q produced no searchable tokens", query)
	}

	root := stringVal(input, "path")
	if root == "" {
		root = workDir
	}
	if root == "" {
		root = "."
	}
	pattern := stringVal(input, "glob")

	topN := semSearchDefaultTopN
	if v, ok := input["max_results"].(float64); ok && v > 0 {
		topN = int(v)
		if topN > semSearchMaxTopN {
			topN = semSearchMaxTopN
		}
	}

	chunks, scanned, truncated, err := semCollectChunks(root, pattern)
	if err != nil {
		return "", fmt.Errorf("semantic_search: %w", err)
	}
	if len(chunks) == 0 {
		return fmt.Sprintf("No indexable files under %s (glob %q).", root, pattern), nil
	}

	rankChunksBM25(chunks, queryTokens)
	sort.SliceStable(chunks, func(i, j int) bool { return chunks[i].score > chunks[j].score })

	var sb strings.Builder
	shown := 0
	for _, c := range chunks {
		if c.score <= 0 {
			break
		}
		fmt.Fprintf(&sb, "%s:%d-%d  (score %.2f)\n    %s\n", c.path, c.startLine, c.endLine, c.score, c.firstLine)
		shown++
		if shown >= topN {
			break
		}
	}
	if shown == 0 {
		return fmt.Sprintf("No relevant chunks for %q across %d files. Try other wording, or grep for exact strings.", query, scanned), nil
	}
	header := fmt.Sprintf("Top %d chunks for %q (%d files ranked):\n", shown, query, scanned)
	if truncated {
		header += "NOTE: scan capped — the tree exceeded the file/byte budget; results cover the scanned subset only.\n"
	}
	return header + sb.String(), nil
}

// semCollectChunks walks root, chunking every indexable file.
func semCollectChunks(root, pattern string) (chunks []*semChunk, filesScanned int, truncated bool, err error) {
	var totalBytes int64
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped, not fatal
		}
		if d.IsDir() {
			if _, skip := semSearchSkipDirs[d.Name()]; skip {
				return filepath.SkipDir
			}
			if name := d.Name(); len(name) > 1 && strings.HasPrefix(name, ".") && name != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if filesScanned >= semSearchMaxFiles || totalBytes >= semSearchMaxBytes {
			truncated = true
			return filepath.SkipAll
		}
		if pattern != "" {
			if ok, _ := filepath.Match(pattern, d.Name()); !ok {
				return nil
			}
		}
		info, err := d.Info()
		if err != nil || info.Size() > semSearchMaxFileSize || info.Size() == 0 {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil || looksBinary(data) {
			return nil
		}
		filesScanned++
		totalBytes += info.Size()

		rel := path
		if r, rerr := filepath.Rel(root, path); rerr == nil {
			rel = r
		}
		chunks = append(chunks, chunkFile(rel, string(data))...)
		return nil
	})
	if walkErr != nil {
		return nil, 0, false, walkErr
	}
	return chunks, filesScanned, truncated, nil
}

// chunkFile splits content into overlapping line windows.
func chunkFile(path, content string) []*semChunk {
	lines := strings.Split(content, "\n")
	var out []*semChunk
	for start := 0; start < len(lines); start += semSearchChunkStride {
		end := start + semSearchChunkLines
		if end > len(lines) {
			end = len(lines)
		}
		window := strings.Join(lines[start:end], "\n")
		tokens := semTokenize(window)
		if len(tokens) == 0 {
			if end == len(lines) {
				break
			}
			continue
		}
		freq := make(map[string]int, len(tokens))
		for _, tok := range tokens {
			freq[tok]++
		}
		first := ""
		for _, l := range lines[start:end] {
			if s := strings.TrimSpace(l); s != "" {
				first = s
				break
			}
		}
		out = append(out, &semChunk{
			path:      path,
			startLine: start + 1,
			endLine:   end,
			tokens:    freq,
			length:    len(tokens),
			firstLine: excerptLine(first, 120),
		})
		if end == len(lines) {
			break
		}
	}
	return out
}

// rankChunksBM25 scores every chunk against the query tokens (k1=1.2, b=0.75).
func rankChunksBM25(chunks []*semChunk, queryTokens []string) {
	const k1, b = 1.2, 0.75
	n := float64(len(chunks))
	var totalLen float64
	df := make(map[string]float64, len(queryTokens))
	for _, c := range chunks {
		totalLen += float64(c.length)
		for _, q := range queryTokens {
			if c.tokens[q] > 0 {
				df[q]++
			}
		}
	}
	avgLen := totalLen / n
	if avgLen == 0 {
		return
	}
	for _, c := range chunks {
		var score float64
		for _, q := range queryTokens {
			tf := float64(c.tokens[q])
			if tf == 0 {
				continue
			}
			idf := math.Log(1 + (n-df[q]+0.5)/(df[q]+0.5))
			score += idf * (tf * (k1 + 1)) / (tf + k1*(1-b+b*float64(c.length)/avgLen))
		}
		c.score = score
	}
}

// semTokenize lowercases and splits text into identifier-aware tokens:
// non-alphanumeric boundaries plus camelCase humps, dropping 1-char tokens
// and a tiny stopword set.
func semTokenize(text string) []string {
	var tokens []string
	var cur []rune
	flush := func() {
		if len(cur) > 1 {
			tok := strings.ToLower(string(cur))
			if _, stop := semStopwords[tok]; !stop {
				tokens = append(tokens, tok)
			}
		}
		cur = cur[:0]
	}
	prevLower := false
	for _, r := range text {
		switch {
		case unicode.IsLetter(r):
			if unicode.IsUpper(r) && prevLower {
				flush() // camelCase hump
			}
			cur = append(cur, r)
			prevLower = unicode.IsLower(r)
		case unicode.IsDigit(r):
			cur = append(cur, r)
			prevLower = false
		default:
			flush()
			prevLower = false
		}
	}
	flush()
	return tokens
}

var semStopwords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "with": {}, "that": {}, "this": {},
	"from": {}, "are": {}, "was": {}, "were": {}, "will": {}, "have": {},
	"has": {}, "not": {}, "but": {}, "you": {}, "your": {}, "where": {},
	"when": {}, "how": {}, "what": {}, "why": {}, "does": {}, "code": {},
	"func": {}, "var": {}, "const": {}, "return": {}, "nil": {}, "err": {},
	"error": {}, "string": {}, "int": {}, "bool": {}, "true": {}, "false": {},
}

func dedupeTokens(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := in[:0]
	for _, t := range in {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// looksBinary reports whether data smells like a binary file (NUL byte in
// the first 8KB).
func looksBinary(data []byte) bool {
	probe := data
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	for _, b := range probe {
		if b == 0 {
			return true
		}
	}
	return false
}

func excerptLine(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
