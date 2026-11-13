package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

var errBlocked = errors.New("blocked")

func decodeRT(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("decode: %v\nraw: %s", err, raw)
	}
	return m
}

func TestRemoteTriggerToolSchema(t *testing.T) {
	tool := RemoteTriggerTool()
	if tool.Name != "remote_trigger" {
		t.Fatalf("name = %q", tool.Name)
	}
	for _, want := range []string{"url", "method", "headers", "body", "json", "timeout_seconds"} {
		if _, ok := tool.InputSchema.Properties[want]; !ok {
			t.Errorf("schema missing %q", want)
		}
	}
}

func TestRemoteTriggerSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type = %q", r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"hello":"world"}` {
			t.Errorf("body = %q", string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", "abc")
		w.Header().Set("Set-Cookie", "session=secret") // must be filtered out
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	out, err := ExecuteRemoteTrigger(context.Background(), map[string]any{
		"url":  server.URL,
		"json": map[string]any{"hello": "world"},
	})
	if err != nil {
		t.Fatal(err)
	}
	res := decodeRT(t, out)
	if res["status_code"].(float64) != 201 {
		t.Errorf("status_code = %v", res["status_code"])
	}
	if res["success"] != false {
		// 201 is success
	}
	if res["body"].(string) != `{"ok":true}` {
		t.Errorf("body = %v", res["body"])
	}
	if res["truncated"] != false {
		t.Errorf("truncated = %v", res["truncated"])
	}
	headers := res["headers"].(map[string]any)
	if _, ok := headers["Set-Cookie"]; ok {
		t.Errorf("Set-Cookie leaked to model context")
	}
	if headers["X-Request-Id"] != "abc" && headers["X-Request-ID"] != "abc" {
		t.Errorf("X-Request-Id missing: %v", headers)
	}
}

func TestRemoteTrigger4xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte("nope"))
	}))
	defer server.Close()

	out, err := ExecuteRemoteTrigger(context.Background(), map[string]any{"url": server.URL, "method": "GET"})
	if err != nil {
		t.Fatal(err)
	}
	res := decodeRT(t, out)
	if res["status_code"].(float64) != 404 {
		t.Fatal("expected 404")
	}
	if res["success"].(bool) {
		t.Fatal("expected success=false on 404")
	}
}

func TestRemoteTrigger5xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
	}))
	defer server.Close()

	out, err := ExecuteRemoteTrigger(context.Background(), map[string]any{"url": server.URL})
	if err != nil {
		t.Fatal(err)
	}
	res := decodeRT(t, out)
	if res["status_code"].(float64) != 503 {
		t.Fatal("expected 503")
	}
}

func TestRemoteTriggerTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
			w.WriteHeader(200)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()

	out, err := ExecuteRemoteTrigger(context.Background(), map[string]any{
		"url":             server.URL,
		"timeout_seconds": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	res := decodeRT(t, out)
	if res["success"].(bool) {
		t.Fatal("expected timeout to surface as success=false")
	}
	if _, ok := res["error"].(string); !ok {
		t.Fatalf("expected error field, got %v", res)
	}
}

func TestRemoteTriggerContextCancel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	out, err := ExecuteRemoteTrigger(ctx, map[string]any{"url": server.URL})
	if err != nil {
		t.Fatal(err)
	}
	res := decodeRT(t, out)
	if res["success"].(bool) {
		t.Fatal("expected cancel to surface as success=false")
	}
}

func TestRemoteTriggerResponseTooLarge(t *testing.T) {
	big := strings.Repeat("x", 4096)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(big))
	}))
	defer server.Close()

	out, err := ExecuteRemoteTriggerWith(context.Background(),
		map[string]any{"url": server.URL, "method": "GET"},
		RemoteTriggerOptions{MaxBodyBytes: 1024},
	)
	if err != nil {
		t.Fatal(err)
	}
	res := decodeRT(t, out)
	if res["truncated"] != true {
		t.Fatal("expected truncated=true")
	}
	if got := res["body"].(string); len(got) != 1024 {
		t.Fatalf("body len = %d, want 1024", len(got))
	}
}

func TestRemoteTriggerHeaderBlocklist(t *testing.T) {
	cases := []string{"Cookie", "cookie", "Proxy-Authorization"}
	for _, h := range cases {
		_, err := ExecuteRemoteTrigger(context.Background(), map[string]any{
			"url":     "https://example.invalid",
			"headers": map[string]any{h: "value"},
		})
		if err == nil {
			t.Errorf("header %q should be rejected", h)
		}
	}
}

func TestRemoteTriggerAuthorizationGated(t *testing.T) {
	t.Run("rejected by default", func(t *testing.T) {
		_, err := ExecuteRemoteTrigger(context.Background(), map[string]any{
			"url":     "https://example.invalid",
			"headers": map[string]any{"Authorization": "Bearer x"},
		})
		if err == nil {
			t.Fatal("Authorization should be rejected by default")
		}
	})
	t.Run("allowed via opts", func(t *testing.T) {
		seen := make(chan string, 1)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen <- r.Header.Get("Authorization")
			w.WriteHeader(200)
		}))
		defer server.Close()

		_, err := ExecuteRemoteTriggerWith(context.Background(),
			map[string]any{"url": server.URL, "headers": map[string]any{"Authorization": "Bearer x"}},
			RemoteTriggerOptions{AllowAuthHeader: true},
		)
		if err != nil {
			t.Fatal(err)
		}
		if got := <-seen; got != "Bearer x" {
			t.Fatalf("server saw Authorization=%q", got)
		}
	})
}

func TestRemoteTriggerHeaderCRLFRejected(t *testing.T) {
	_, err := ExecuteRemoteTrigger(context.Background(), map[string]any{
		"url":     "https://example.invalid",
		"headers": map[string]any{"X-Inject": "good\r\nEvil: yes"},
	})
	if err == nil {
		t.Fatal("CRLF in header value must be rejected")
	}
}

func TestRemoteTriggerRejectsBadScheme(t *testing.T) {
	_, err := ExecuteRemoteTrigger(context.Background(), map[string]any{"url": "file:///etc/passwd"})
	if err == nil {
		t.Fatal("file:// scheme should be rejected")
	}
	_, err = ExecuteRemoteTrigger(context.Background(), map[string]any{"url": "ftp://example.com/x"})
	if err == nil {
		t.Fatal("ftp:// scheme should be rejected")
	}
}

func TestRemoteTriggerRejectsBodyAndJSONTogether(t *testing.T) {
	_, err := ExecuteRemoteTrigger(context.Background(), map[string]any{
		"url":  "https://example.invalid",
		"body": "raw",
		"json": map[string]any{"a": 1},
	})
	if err == nil {
		t.Fatal("body + json together should be rejected")
	}
}

func TestRemoteTriggerRejectsUnknownMethod(t *testing.T) {
	_, err := ExecuteRemoteTrigger(context.Background(), map[string]any{
		"url":    "https://example.invalid",
		"method": "FOO",
	})
	if err == nil {
		t.Fatal("unknown method should be rejected")
	}
}

func TestRemoteTriggerURLValidator(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer server.Close()

	_, err := ExecuteRemoteTriggerWith(context.Background(),
		map[string]any{"url": server.URL},
		RemoteTriggerOptions{
			URLValidator: func(_ *url.URL) error { return errBlocked },
		},
	)
	if err == nil {
		t.Fatal("URL validator rejection should surface as error")
	}
}
