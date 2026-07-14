package delegate

import (
	"encoding/json"
	"testing"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

func TestCodexLocalImageUsesAppServerWireType(t *testing.T) {
	content := codexsdk.Blocks(
		codexsdk.TextInput("describe the reference"),
		codexsdk.LocalImageInput("/tmp/reference.png"),
	)

	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal codex content: %v", err)
	}

	var blocks []map[string]any
	if err := json.Unmarshal(data, &blocks); err != nil {
		t.Fatalf("unmarshal codex content: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("got %d content blocks, want 2: %s", len(blocks), data)
	}
	if got := blocks[1]["type"]; got != "localImage" {
		t.Fatalf("local image type = %q, want %q (turn/start input: %s)", got, "localImage", data)
	}
	if got := blocks[1]["path"]; got != "/tmp/reference.png" {
		t.Fatalf("local image path = %q, want %q", got, "/tmp/reference.png")
	}
}
