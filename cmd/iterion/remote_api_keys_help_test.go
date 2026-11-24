package main

import (
	"strings"
	"testing"
)

// TestRemoteAPIKeysCreateHelp_NamesEveryProviderID: the setup prompt says
// `--provider <provider>` and leaves the id to the agent, so `api-keys create
// --help` is where an agent holding a Kimi or a z.ai key learns to type
// `moonshot` or `zai`. The ids are spelled out here rather than read from
// the list the help is built from: a help that stops naming them stays red.
func TestRemoteAPIKeysCreateHelp_NamesEveryProviderID(t *testing.T) {
	flag := remoteAPIKeysCreateCmd.Flags().Lookup("provider")
	if flag == nil {
		t.Fatal("`api-keys create` has no --provider flag")
	}
	list, ok := strings.CutPrefix(flag.Usage, "LLM provider: ")
	if !ok {
		t.Fatalf("--provider help does not open on its id list: %s", flag.Usage)
	}
	list, _, _ = strings.Cut(list, " ")
	named := map[string]bool{}
	for _, id := range strings.Split(list, "|") {
		named[id] = true
	}
	for _, id := range []string{
		"anthropic", "openai", "xai", "zai", "moonshot",
		"openrouter", "bedrock", "vertex", "azure", "openai_compatible",
	} {
		if !named[id] {
			t.Errorf("--provider help does not name %q: %s", id, flag.Usage)
		}
	}
	if !strings.Contains(flag.Usage, "moonshot = Kimi") {
		t.Errorf("--provider help does not say which id reaches Kimi: %s", flag.Usage)
	}
}
