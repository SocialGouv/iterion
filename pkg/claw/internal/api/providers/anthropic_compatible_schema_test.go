package providers_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/providers/moonshot"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/providers/zai"
)

func TestCompatibleSchemaRepairs(t *testing.T) {
	for _, provider := range []api.Provider{zai.New(), moonshot.New()} {
		t.Run(provider.Name(), func(t *testing.T) {
			bodies := make(chan []byte, 4)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				bodies <- b
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			}))
			defer srv.Close()
			c, err := provider.NewClient(api.ProviderConfig{APIKey: "test", BaseURL: srv.URL, Model: "test", NoAmbientHeaders: true})
			if err != nil {
				t.Fatal(err)
			}
			var s api.InputSchema
			if err := json.Unmarshal([]byte("{\"type\":\"object\",\"properties\":{\"n\":{\"description\":null,\"const\":null}},\"required\":null,\"pattern\":\"\xff\xfe\"}"), &s); err != nil {
				t.Fatal(err)
			}
			events, err := c.StreamResponse(t.Context(), api.CreateMessageRequest{Tools: []api.Tool{{Name: "test", InputSchema: s}}})
			if err != nil {
				t.Fatal(err)
			}
			for range events {
			}
			body := <-bodies
			if !utf8.Valid(body) {
				t.Error("HTTP body is not UTF-8")
			}
			if strings.Contains(string(body), `"description":null`) || strings.Contains(string(body), `"required":null`) || !strings.Contains(string(body), `"const":null`) {
				t.Errorf("null keyword repair: %s", body)
			}
		})
	}
}
