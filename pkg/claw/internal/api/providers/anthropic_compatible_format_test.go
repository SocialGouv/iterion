package providers_test

import (
	"errors"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/providers/moonshot"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/providers/zai"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestCompatibleResponseFormatRefusal(t *testing.T) {
	for _, provider := range []api.Provider{zai.New(), moonshot.New()} {
		t.Run(provider.Name(), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
			}))
			defer srv.Close()
			c, err := provider.NewClient(api.ProviderConfig{APIKey: "test", BaseURL: srv.URL, Model: "test", NoAmbientHeaders: true})
			if err != nil {
				t.Fatal(err)
			}
			events, err := c.StreamResponse(t.Context(), api.CreateMessageRequest{ResponseFormat: &api.ResponseFormat{Type: "json_object"}})
			if events != nil {
				for range events {
				}
			}
			var invalid *api.RequestValidationError
			if !errors.As(err, &invalid) {
				t.Errorf("ResponseFormat silently dropped: %T %v", err, err)
			}
			if calls.Load() != 0 {
				t.Error("unsupported format reached HTTP")
			}
		})
	}
}
