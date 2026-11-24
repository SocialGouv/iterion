package openai

import (
	"encoding/json"
	"reflect"
	"testing"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/providers/openaiwire"
)

func TestSchemaRepairsOnWire(t *testing.T) {
	for _, mode := range []string{"chat", "responses"} {
		_ = mode
		cases := []struct{ name, input, want string }{
			{"null-keywords", `{"type":"object","properties":{"n":{"description":null,"enum":null,"items":null,"const":null,"default":null,"examples":null},"e":{"enum":[null,{"description":null}]}},"required":null,"allOf":[{"description":null}],"$defs":{"x":{"items":null}},"x-custom":{"description":null}}`, `{"type":"object","properties":{"n":{"const":null,"default":null,"items":{}},"e":{"enum":[null,{"description":null}]}},"allOf":[{}],"$defs":{"x":{"items":{}}},"x-custom":{"description":null}}`},
			{"utf8", "{\"type\":\"object\",\"properties\":{},\"pattern\":\"\xff\xfe\"}", `{"type":"object","properties":{},"pattern":"\ufffd\ufffd"}`},
		}
		for _, input := range []string{`{}`, `{"properties":{}}`, `{"type":null}`, `{"type":""}`, `{"type":"object","properties":null}`, `{"type":"object","required":null}`, `null`, `{"type":"object","additionalProperties":false}`} {
			want := `{"type":"object","properties":{}}`
			if input == `{"type":"object","additionalProperties":false}` {
				want = `{"type":"object","properties":{},"additionalProperties":false}`
			}
			cases = append(cases, struct{ name, input, want string }{"root-" + input, input, want})
		}
		for _, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				var s api.InputSchema
				if err := json.Unmarshal([]byte(tc.input), &s); err != nil {
					t.Fatal(err)
				}
				req := api.CreateMessageRequest{Tools: []api.Tool{{Name: "witness", InputSchema: s}}}
				var body []byte
				var err error
				if mode == "chat" {
					r, e := (&Client{}).buildRequest(req)
					err = e
					if e == nil {
						body, err = openaiwire.MarshalJSON(r)
					}
				} else {
					r, e := (&Client{}).buildResponsesRequest(req)
					err = e
					if e == nil {
						body, err = openaiwire.MarshalJSON(r)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				if !utf8.Valid(body) {
					t.Errorf("HTTP body is not UTF-8: %q", body)
				}
				var envelope map[string]any
				if err := json.Unmarshal(body, &envelope); err != nil {
					t.Fatal(err)
				}
				tool := envelope["tools"].([]any)[0].(map[string]any)
				var got any
				if mode == "chat" {
					tool = tool["function"].(map[string]any)
				}
				got = tool["parameters"]
				var want any
				if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("schema got %#v, want %#v", got, want)
				}
			})
		}
	}
}
