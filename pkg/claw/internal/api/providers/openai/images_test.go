package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

func imageTestPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestImageGenerationCapability(t *testing.T) {
	for _, tc := range []struct {
		cfg  api.ProviderConfig
		want bool
	}{
		{api.ProviderConfig{APIKey: "sk-test"}, true},
		{api.ProviderConfig{APIKey: "sk-test", BaseURL: "https://compatible.example/v1"}, false},
		{api.ProviderConfig{OAuthToken: "oauth-test", OpenAIChatGPTAccountID: "acct-test"}, true},
	} {
		client, err := New().NewClient(tc.cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got := client.(*Client).SupportsImageGeneration(); got != tc.want {
			t.Fatalf("capability for %+v = %v, want %v", tc.cfg, got, tc.want)
		}
	}
}

func TestGenerateImageHasExtendedHeaderBudget(t *testing.T) {
	client, err := New().NewClient(api.ProviderConfig{APIKey: "sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	c := client.(*Client)
	streaming := c.HTTPClient.Transport.(*http.Transport).ResponseHeaderTimeout
	images := c.ImageHTTPClient.Transport.(*http.Transport).ResponseHeaderTimeout
	if streaming != time.Minute || images != imageRequestTimeout || images <= streaming {
		t.Fatalf("header timeouts: streaming %v, images %v", streaming, images)
	}
}

func TestGenerateImageEndpointsAndAuth(t *testing.T) {
	for _, tc := range []struct {
		name, suffix          string
		cfg                   api.ProviderConfig
		wantAuth, wantAccount string
	}{
		{"api_key", "/v1/images/generations", api.ProviderConfig{APIKey: "sk-test"}, "Bearer sk-test", ""},
		{"codex_oauth", "/images/generations", api.ProviderConfig{OAuthToken: "oauth-test", OpenAIChatGPTAccountID: "acct-test"}, "Bearer oauth-test", "acct-test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pngData := imageTestPNG(t)
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != tc.suffix || r.Method != http.MethodPost {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != tc.wantAuth {
					t.Errorf("Authorization = %q", got)
				}
				if got := r.Header.Get("ChatGPT-Account-ID"); got != tc.wantAccount {
					t.Errorf("ChatGPT-Account-ID = %q", got)
				}
				if tc.wantAccount != "" && r.Header.Get("x-codex-image-turn-id") == "" {
					t.Error("missing Codex image turn ID")
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["prompt"] != "a red square" || body["model"] != defaultImageModel {
					t.Errorf("body = %#v", body)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"created": 1, "data": []any{map[string]string{"b64_json": base64.StdEncoding.EncodeToString(pngData)}}})
			}))
			defer srv.Close()
			tc.cfg.BaseURL = srv.URL
			client, err := New().NewClient(tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.(api.ImageGenerator).GenerateImage(context.Background(), "a red square")
			if err != nil || !bytes.Equal(got.Data, pngData) || calls != 1 {
				t.Fatalf("GenerateImage = %d bytes, %v; calls = %d", len(got.Data), err, calls)
			}
		})
	}
}

func TestGenerateImageRereadsCodexCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	writeAuth := func(token, account string) {
		data, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "tokens": map[string]string{"access_token": token, "account_id": account}})
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeAuth("first", "acct-1")
	var observed []string
	pngData := imageTestPNG(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed = append(observed, r.Header.Get("Authorization")+"/"+r.Header.Get("ChatGPT-Account-ID"))
		_, _ = io.WriteString(w, `{"created":1,"data":[{"b64_json":"`+base64.StdEncoding.EncodeToString(pngData)+`"}]}`)
	}))
	defer srv.Close()
	client, err := New().NewClient(api.ProviderConfig{CodexAuthFile: path, BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	g := client.(api.ImageGenerator)
	if _, err := g.GenerateImage(context.Background(), "first image"); err != nil {
		t.Fatal(err)
	}
	writeAuth("second", "acct-2")
	if _, err := g.GenerateImage(context.Background(), "second image"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(observed, ",") != "Bearer first/acct-1,Bearer second/acct-2" {
		t.Fatalf("fresh credentials not applied: %v", observed)
	}
}

func TestGenerateImageErrorsAreBoundedAndSanitized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"secret-access-token"}}`)
	}))
	defer srv.Close()
	client, err := New().NewClient(api.ProviderConfig{APIKey: "sk-test", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.(api.ImageGenerator).GenerateImage(context.Background(), "test")
	if err == nil || strings.Contains(err.Error(), "secret-access-token") {
		t.Fatalf("unsafe error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.(api.ImageGenerator).GenerateImage(ctx, "test")
	if err == nil {
		t.Fatal("cancelled image request succeeded")
	}
}
