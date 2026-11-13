package runtime

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/plugin"
)

// Candidate witness through the REAL site (InitPlugins): a plugin tool whose
// manifest schema omits "type" reaches the wire with type "object", as the
// site's explicit default intends.
func TestWitnessInitPluginsTypeDefaultReachesWire(t *testing.T) {
	reg := plugin.NewPluginRegistry([]plugin.RegisteredPlugin{{
		Enabled: true,
		Plugin: &plugin.BuiltinPlugin{
			Meta: plugin.PluginMetadata{ID: "p", Name: "p"},
			ToolsDef: []plugin.PluginToolDefinition{{
				Name: "t", Description: "d",
				InputSchema: json.RawMessage(`{"properties":{"x":{"type":"string","pattern":"x+"}},"additionalProperties":false,"const":9007199254740993}`),
			}},
		},
	}})
	loop := &ConversationLoop{}
	loop.InitPlugins(reg)
	var found bool
	for _, tool := range loop.Tools {
		if tool.Name != "t" {
			continue
		}
		found = true
		wire, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(wire), `"additionalProperties":false`) || !strings.Contains(string(wire), `"const":9007199254740993`) || !strings.Contains(string(wire), `"pattern":"x+"`) {
			t.Fatalf("plugin default lost keywords: %s", wire)
		}
		if tool.InputSchema.Type != "object" {
			t.Fatalf("typed default not applied: %+v", tool.InputSchema)
		}
		if !strings.Contains(string(wire), `"type":"object"`) {
			t.Fatalf("site default lost on the wire: typed Type=%q, wire=%s", tool.InputSchema.Type, wire)
		}
	}
	if !found {
		t.Fatal("plugin tool not registered")
	}
}
