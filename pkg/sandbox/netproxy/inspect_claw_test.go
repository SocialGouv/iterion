package netproxy_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	clawapi "github.com/SocialGouv/claw-code-go/pkg/api"
	clawanthropic "github.com/SocialGouv/claw-code-go/pkg/api/providers/anthropic"
	clawmoonshot "github.com/SocialGouv/claw-code-go/pkg/api/providers/moonshot"
	clawopenai "github.com/SocialGouv/claw-code-go/pkg/api/providers/openai"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/sandbox/netproxy"
)

// childPlaceholderEnv switches the test binary into the child that runs the
// claw clients.
const childPlaceholderEnv = "NETPROXY_CLAW_CHILD_PLACEHOLDER"

// The sandboxed claw runner's own model clients — claw-code-go's providers,
// through HTTPS_PROXY and the per-run CA as a sandbox wires them — reach each
// model endpoint they call with the conversation in placeholder form: the
// ChatGPT forfait, OpenAI with an API key, Anthropic, Moonshot. The clients
// run in a child process: Go loads its trust store once per process, from
// SSL_CERT_FILE.
func TestTheClawClientsModelCallsKeepTheirPlaceholders(t *testing.T) {
	if ph := os.Getenv(childPlaceholderEnv); ph != "" {
		callClawClients(ph)
		return
	}
	const token = "ghp_R3alV4lu3Fak3T0k3nAbCdEfGhIjKlMn0p"
	g := secretguard.New([]secretguard.Secret{{Name: "github_token", Value: token}}, secretguard.DefaultConfig())
	ph := g.Secrets()[0].Placeholder
	var mu sync.Mutex
	bodies := map[string]string{}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies[r.Host+r.URL.Path] = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(upstream.Close)
	addr := upstream.Listener.Addr().String()
	pool := x509.NewCertPool()
	pool.AddCert(upstream.Certificate())
	ca, err := netproxy.NewEphemeralCA()
	if err != nil {
		t.Fatal(err)
	}
	pol, err := netproxy.Compile(netproxy.ModeOpen, nil)
	if err != nil {
		t.Fatal(err)
	}
	prx, err := netproxy.New(netproxy.Options{Policy: pol, InspectCA: ca, Rewriter: g,
		InspectUpstreamTLS: &tls.Config{RootCAs: pool, ServerName: "example.com"},
		Dial:               func(_ context.Context, network, _ string) (net.Conn, error) { return net.Dial(network, addr) }})
	if err != nil {
		t.Fatal(err)
	}
	if err := prx.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = prx.Shutdown(ctx)
	})
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, ca.CertPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	proxyURL := "http://" + prx.Addr().String()
	child := exec.Command(os.Args[0], "-test.run=^TestTheClawClientsModelCallsKeepTheirPlaceholders$", "-test.count=1")
	child.Env = append(os.Environ(), childPlaceholderEnv+"="+ph, "SSL_CERT_FILE="+caFile, "SSL_CERT_DIR=",
		"HTTPS_PROXY="+proxyURL, "https_proxy="+proxyURL, "HTTP_PROXY="+proxyURL, "http_proxy="+proxyURL, "NO_PROXY=", "no_proxy=")
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("the claw clients' child: %v\n%s", err, out)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, endpoint := range []string{
		"chatgpt.com/backend-api/codex/responses",
		"api.openai.com/v1/chat/completions",
		"api.anthropic.com/v1/messages",
		"api.moonshot.ai/anthropic/v1/messages",
	} {
		body, ok := bodies[endpoint]
		if !ok {
			t.Errorf("%s: never reached (reached: %v)", endpoint, keys(bodies))
			continue
		}
		if strings.Contains(body, token) || !strings.Contains(body, ph) {
			t.Errorf("%s: the provider received the conversation materialised: %.300s", endpoint, body)
		}
	}
}

// callClawClients streams one request from each claw client whose
// conversation names a secret by its placeholder; answers do not matter.
func callClawClients(ph string) {
	conversation := []clawapi.Message{{Role: "user", Content: []clawapi.ContentBlock{{Type: "text", Text: "Push the branch with " + ph}}}}
	for _, c := range []struct {
		provider clawapi.Provider
		cfg      clawapi.ProviderConfig
		model    string
	}{
		{clawopenai.New(), clawapi.ProviderConfig{OAuthToken: "fake-oauth-access-token", OpenAIChatGPTAccountID: "acct-fake", Model: "gpt-5.5"}, "gpt-5.5"},
		{clawopenai.New(), clawapi.ProviderConfig{APIKey: "sk-fake", Model: "gpt-5.5"}, "gpt-5.5"},
		{clawanthropic.New(), clawapi.ProviderConfig{APIKey: "sk-ant-fake", Model: "claude-sonnet-4-5"}, "claude-sonnet-4-5"},
		{clawmoonshot.New(), clawapi.ProviderConfig{APIKey: "sk-moonshot-fake", Model: "kimi-k2"}, "kimi-k2"},
	} {
		client, err := c.provider.NewClient(c.cfg)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if ch, err := client.StreamResponse(ctx, clawapi.CreateMessageRequest{Model: c.model, MaxTokens: 64, Messages: conversation, Stream: true}); err == nil {
			for range ch {
			}
		}
		cancel()
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
