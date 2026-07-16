package delegate

import (
	"encoding/json"
	"testing"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

func TestCodexLocalImageInputUsesAppServerDiscriminator(t *testing.T) {
	data, err := json.Marshal(codexsdk.LocalImageInput("/tmp/reference.png"))
	if err != nil {
		t.Fatalf("marshal local image input: %v", err)
	}

	const want = `{"type":"localImage","path":"/tmp/reference.png"}`
	if string(data) != want {
		t.Fatalf("unexpected local image payload: got %s, want %s", data, want)
	}
}
