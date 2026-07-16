package subprocess

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// mockChunkReader delivers data in controlled chunks to simulate various buffering scenarios.
type mockChunkReader struct {
	chunks [][]byte
	index  int
}

func newMockChunkReader(chunks ...string) *mockChunkReader {
	byteChunks := make([][]byte, len(chunks))
	for i, chunk := range chunks {
		byteChunks[i] = []byte(chunk)
	}

	return &mockChunkReader{chunks: byteChunks}
}

func (r *mockChunkReader) Read(p []byte) (int, error) {
	if r.index >= len(r.chunks) {
		return 0, io.EOF
	}

	chunk := r.chunks[r.index]
	r.index++

	n := copy(p, chunk)

	return n, nil
}

// TestMultipleJSONObjectsOnSingleLine tests parsing when multiple JSON objects
// are delivered in a single read but separated by newlines.
func TestMultipleJSONObjectsOnSingleLine(t *testing.T) {
	jsonObj1 := map[string]any{"type": "message", "id": "msg1", "content": "First message"}
	jsonObj2 := map[string]any{"type": "result", "id": "res1", "status": "completed"}

	json1, err := json.Marshal(jsonObj1)
	require.NoError(t, err)

	json2, err := json.Marshal(jsonObj2)
	require.NoError(t, err)

	bufferedLine := string(json1) + "\n" + string(json2) + "\n"

	reader := newMockChunkReader(bufferedLine)
	messages := parseJSONLines(t, reader)

	require.Len(t, messages, 2)
	require.Equal(t, "message", messages[0]["type"])
	require.Equal(t, "msg1", messages[0]["id"])
	require.Equal(t, "result", messages[1]["type"])
	require.Equal(t, "res1", messages[1]["id"])
}

// TestJSONWithEmbeddedNewlines tests parsing JSON objects that contain
// newline characters in string values (which are escaped as \n in JSON).
func TestJSONWithEmbeddedNewlines(t *testing.T) {
	jsonObj1 := map[string]any{"type": "message", "content": "Line 1\nLine 2\nLine 3"}
	jsonObj2 := map[string]any{"type": "result", "data": "Some\nMultiline\nContent"}

	json1, err := json.Marshal(jsonObj1)
	require.NoError(t, err)

	json2, err := json.Marshal(jsonObj2)
	require.NoError(t, err)

	bufferedLine := string(json1) + "\n" + string(json2) + "\n"

	reader := newMockChunkReader(bufferedLine)
	messages := parseJSONLines(t, reader)

	require.Len(t, messages, 2)
	require.Equal(t, "Line 1\nLine 2\nLine 3", messages[0]["content"])
	require.Equal(t, "Some\nMultiline\nContent", messages[1]["data"])
}

// TestMultipleNewlinesBetweenObjects tests parsing with multiple blank lines
// between JSON objects.
func TestMultipleNewlinesBetweenObjects(t *testing.T) {
	jsonObj1 := map[string]any{"type": "message", "id": "msg1"}
	jsonObj2 := map[string]any{"type": "result", "id": "res1"}

	json1, err := json.Marshal(jsonObj1)
	require.NoError(t, err)

	json2, err := json.Marshal(jsonObj2)
	require.NoError(t, err)

	bufferedLine := string(json1) + "\n\n\n" + string(json2) + "\n"

	reader := newMockChunkReader(bufferedLine)
	messages := parseJSONLinesSkipEmpty(t, reader)

	require.Len(t, messages, 2)
	require.Equal(t, "msg1", messages[0]["id"])
	require.Equal(t, "res1", messages[1]["id"])
}

// TestSplitJSONAcrossMultipleReads tests parsing when a single JSON object
// is split across multiple stream reads.
func TestSplitJSONAcrossMultipleReads(t *testing.T) {
	jsonObj := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": strings.Repeat("x", 1000)},
				map[string]any{
					"type":  "tool_use",
					"id":    "tool_123",
					"name":  "Read",
					"input": map[string]any{"file_path": "/test.txt"},
				},
			},
		},
	}

	completeJSON, err := json.Marshal(jsonObj)
	require.NoError(t, err)

	completeJSON = append(completeJSON, '\n')

	part1 := string(completeJSON[:100])
	part2 := string(completeJSON[100:250])
	part3 := string(completeJSON[250:])

	reader := newMockChunkReader(part1, part2, part3)
	messages := parseJSONLines(t, reader)

	require.Len(t, messages, 1)
	require.Equal(t, "assistant", messages[0]["type"])

	msgContent, ok := messages[0]["message"].(map[string]any)
	require.True(t, ok)

	content, ok := msgContent["content"].([]any)
	require.True(t, ok)
	require.Len(t, content, 2)
}

// TestLargeMinifiedJSON tests parsing a large minified JSON that may be split
// across multiple 64KB chunks.
func TestLargeMinifiedJSON(t *testing.T) {
	largeData := make([]map[string]any, 1000)
	for i := range largeData {
		largeData[i] = map[string]any{
			"id":    i,
			"value": strings.Repeat("x", 100),
		}
	}

	jsonObj := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{
					"tool_use_id": "toolu_016fed1NhiaMLqnEvrj5NUaj",
					"type":        "tool_result",
					"content":     largeData,
				},
			},
		},
	}

	completeJSON, err := json.Marshal(jsonObj)
	require.NoError(t, err)

	completeJSON = append(completeJSON, '\n')

	chunkSize := 64 * 1024

	var chunks []string

	for i := 0; i < len(completeJSON); i += chunkSize {
		end := min(i+chunkSize, len(completeJSON))
		chunks = append(chunks, string(completeJSON[i:end]))
	}

	reader := newMockChunkReader(chunks...)
	messages := parseJSONLines(t, reader)

	require.Len(t, messages, 1)
	require.Equal(t, "user", messages[0]["type"])

	msg, ok := messages[0]["message"].(map[string]any)
	require.True(t, ok)

	content, ok := msg["content"].([]any)
	require.True(t, ok)
	require.Len(t, content, 1)

	toolResult, ok := content[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "toolu_016fed1NhiaMLqnEvrj5NUaj", toolResult["tool_use_id"])
}

// TestBufferSizeExceeded tests that exceeding the scanner buffer size returns an error.
func TestBufferSizeExceeded(t *testing.T) {
	customLimit := 1024
	hugeContent := strings.Repeat("x", customLimit+100)
	incompleteJSON := `{"data": "` + hugeContent + `"}` + "\n"

	reader := strings.NewReader(incompleteJSON)

	scanner := bufio.NewScanner(reader)

	buf := make([]byte, customLimit)
	scanner.Buffer(buf, customLimit)

	scanned := scanner.Scan()
	require.False(t, scanned)
	require.Error(t, scanner.Err())
	require.Contains(t, scanner.Err().Error(), "token too long")
}

// TestBufferSizeOption tests that the configurable buffer size option is respected.
func TestBufferSizeOption(t *testing.T) {
	customLimit := 512
	validContent := strings.Repeat("x", customLimit-100)
	validJSON := `{"data": "` + validContent + `"}` + "\n"

	reader := strings.NewReader(validJSON)
	scanner := bufio.NewScanner(reader)

	buf := make([]byte, customLimit)
	scanner.Buffer(buf, customLimit)

	require.True(t, scanner.Scan())
	require.NoError(t, scanner.Err())

	var msg map[string]any

	err := json.Unmarshal(scanner.Bytes(), &msg)
	require.NoError(t, err)
	require.Equal(t, validContent, msg["data"])
}

// TestMixedCompleteAndSplitJSON tests handling a mix of complete and split JSON messages.
func TestMixedCompleteAndSplitJSON(t *testing.T) {
	msg1 := map[string]any{"type": "system", "subtype": "start"}

	largeMsg := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": strings.Repeat("y", 5000)},
			},
		},
	}

	msg3 := map[string]any{"type": "system", "subtype": "end"}

	json1, err := json.Marshal(msg1)
	require.NoError(t, err)

	largeJSON, err := json.Marshal(largeMsg)
	require.NoError(t, err)

	json3, err := json.Marshal(msg3)
	require.NoError(t, err)

	lines := []string{
		string(json1) + "\n",
		string(largeJSON[:1000]),
		string(largeJSON[1000:3000]),
		string(largeJSON[3000:]) + "\n" + string(json3) + "\n",
	}

	reader := newMockChunkReader(lines...)
	messages := parseJSONLines(t, reader)

	require.Len(t, messages, 3)
	require.Equal(t, "system", messages[0]["type"])
	require.Equal(t, "start", messages[0]["subtype"])
	require.Equal(t, "assistant", messages[1]["type"])
	require.Equal(t, "system", messages[2]["type"])
	require.Equal(t, "end", messages[2]["subtype"])

	assistantMsg, ok := messages[1]["message"].(map[string]any)
	require.True(t, ok)

	content, ok := assistantMsg["content"].([]any)
	require.True(t, ok)
	require.Len(t, content, 1)

	textBlock, ok := content[0].(map[string]any)
	require.True(t, ok)

	text, ok := textBlock["text"].(string)
	require.True(t, ok)
	require.Len(t, text, 5000)
}

// parseJSONLines is a helper that mimics the transport's JSON parsing logic.
func parseJSONLines(t *testing.T, reader io.Reader) []map[string]any {
	t.Helper()

	var messages []map[string]any

	scanner := bufio.NewScanner(reader)

	buf := make([]byte, maxScanTokenSize)
	scanner.Buffer(buf, maxScanTokenSize)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var msg map[string]any

		if err := json.Unmarshal(line, &msg); err != nil {
			t.Fatalf("Failed to unmarshal JSON: %v, line: %s", err, string(line))
		}

		messages = append(messages, msg)
	}

	require.NoError(t, scanner.Err())

	return messages
}

// parseJSONLinesSkipEmpty is a helper that skips empty lines during parsing.
func parseJSONLinesSkipEmpty(t *testing.T, reader io.Reader) []map[string]any {
	t.Helper()

	var messages []map[string]any

	scanner := bufio.NewScanner(reader)

	buf := make([]byte, maxScanTokenSize)
	scanner.Buffer(buf, maxScanTokenSize)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var msg map[string]any

		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}

		messages = append(messages, msg)
	}

	require.NoError(t, scanner.Err())

	return messages
}
