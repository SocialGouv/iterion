package otlpgrpc

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	otellog "go.opentelemetry.io/otel/log"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/SocialGouv/iterion/pkg/claw/internal/apikit"
)

// fakeCollector is an in-memory gRPC server implementing the OTLP logs
// service. Tests configure its response code per case and snapshot the
// payloads it received.
type fakeCollector struct {
	collogspb.UnimplementedLogsServiceServer

	mu       sync.Mutex
	requests []*collogspb.ExportLogsServiceRequest
	calls    atomic.Int32

	respond func(call int32) (*collogspb.ExportLogsServiceResponse, error)
}

func (f *fakeCollector) Export(ctx context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	n := f.calls.Add(1)
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if f.respond == nil {
		return &collogspb.ExportLogsServiceResponse{}, nil
	}
	return f.respond(n)
}

func (f *fakeCollector) snapshot() []*collogspb.ExportLogsServiceRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*collogspb.ExportLogsServiceRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

type collectorHarness struct {
	t        *testing.T
	server   *grpc.Server
	listener *bufconn.Listener
	dialer   func(context.Context, string) (net.Conn, error)
	fake     *fakeCollector
}

func newCollector(t *testing.T) *collectorHarness {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	fake := &fakeCollector{}
	collogspb.RegisterLogsServiceServer(srv, fake)

	go func() {
		if err := srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Logf("server.Serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		srv.GracefulStop()
	})

	return &collectorHarness{
		t:        t,
		server:   srv,
		listener: lis,
		dialer: func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		},
		fake: fake,
	}
}

func (h *collectorHarness) exporterOpts(t *testing.T) []otlploggrpc.Option {
	t.Helper()
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(h.dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return []otlploggrpc.Option{otlploggrpc.WithGRPCConn(conn)}
}

func makeExporter(t *testing.T, h *collectorHarness, mutate ...func(*Config)) *Exporter {
	t.Helper()
	cfg := Config{
		Endpoint:             "127.0.0.1:1",
		Insecure:             true,
		BatchSize:            5,
		FlushInterval:        50 * time.Millisecond,
		ExportTimeout:        2 * time.Second,
		ExtraExporterOptions: h.exporterOpts(t),
	}
	for _, m := range mutate {
		m(&cfg)
	}
	exp, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := exp.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = exp.Stop(ctx)
	})
	return exp
}

func TestExporter_BatchesAndFlushesOnInterval(t *testing.T) {
	h := newCollector(t)
	exp := makeExporter(t, h, func(c *Config) {
		c.BatchSize = 1000
		c.FlushInterval = 80 * time.Millisecond
	})

	exp.Record(apikit.TelemetryEvent{Type: apikit.EventTypeAnalytics, SessionID: "s1"})
	exp.Record(apikit.TelemetryEvent{Type: apikit.EventTypeAnalytics, SessionID: "s2"})

	if !waitFor(t, 3*time.Second, func() bool { return h.fake.calls.Load() >= 1 }) {
		t.Fatalf("interval flush did not happen, calls=%d", h.fake.calls.Load())
	}
	totalRecords := countRecords(h.fake.snapshot())
	if totalRecords < 2 {
		t.Errorf("expected at least 2 LogRecords delivered, got %d", totalRecords)
	}
}

func TestExporter_ForceFlush_DeliversPending(t *testing.T) {
	h := newCollector(t)
	exp := makeExporter(t, h, func(c *Config) {
		c.BatchSize = 1000
		c.FlushInterval = 1 * time.Hour
	})

	for i := 0; i < 4; i++ {
		exp.Record(apikit.TelemetryEvent{
			Type:      apikit.EventTypeHTTPRequestSucceeded,
			SessionID: "abc",
			Method:    "POST",
			Path:      "/v1/messages",
			Status:    200,
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := exp.ForceFlush(ctx); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
	if got := countRecords(h.fake.snapshot()); got != 4 {
		t.Errorf("expected 4 records delivered after ForceFlush, got %d", got)
	}
}

func TestExporter_ResourceAttributes(t *testing.T) {
	h := newCollector(t)
	exp := makeExporter(t, h, func(c *Config) {
		c.ServiceName = "claw-code-go-test"
		c.ServiceVersion = "0.1.2"
		c.BatchSize = 1
	})

	exp.Record(apikit.TelemetryEvent{Type: apikit.EventTypeAnalytics, SessionID: "abc"})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := exp.ForceFlush(ctx); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}

	reqs := h.fake.snapshot()
	if len(reqs) == 0 {
		t.Fatal("no requests captured")
	}
	if !hasResourceAttr(reqs[0], "service.name", "claw-code-go-test") {
		t.Errorf("expected service.name=claw-code-go-test in resource attrs, got %v", reqs[0].ResourceLogs)
	}
	if !hasResourceAttr(reqs[0], "service.version", "0.1.2") {
		t.Errorf("expected service.version=0.1.2 in resource attrs, got %v", reqs[0].ResourceLogs)
	}
}

func TestExporter_StopFlushesPending(t *testing.T) {
	h := newCollector(t)
	exp := makeExporter(t, h, func(c *Config) {
		c.BatchSize = 1000
		c.FlushInterval = 1 * time.Hour
	})

	exp.Record(apikit.TelemetryEvent{Type: apikit.EventTypeAnalytics})
	exp.Record(apikit.TelemetryEvent{Type: apikit.EventTypeAnalytics})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := exp.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := countRecords(h.fake.snapshot()); got != 2 {
		t.Errorf("expected 2 records flushed by Stop, got %d", got)
	}
}

func TestExporter_StopIsIdempotent(t *testing.T) {
	h := newCollector(t)
	exp := makeExporter(t, h)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := exp.Stop(ctx); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := exp.Stop(ctx); err != nil {
		t.Errorf("second Stop must be a no-op, got %v", err)
	}
}

func TestExporter_RecordAfterStop_DoesNotPanic(t *testing.T) {
	h := newCollector(t)
	exp := makeExporter(t, h)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := exp.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Record panicked after Stop: %v", r)
				}
			}()
			exp.Record(apikit.TelemetryEvent{Type: apikit.EventTypeAnalytics})
		}()
	}
	wg.Wait()
}

func TestExporter_RetriesOnTransient(t *testing.T) {
	h := newCollector(t)
	h.fake.respond = func(call int32) (*collogspb.ExportLogsServiceResponse, error) {
		if call == 1 {
			return nil, status.Error(codes.Unavailable, "kicking the tires")
		}
		return &collogspb.ExportLogsServiceResponse{}, nil
	}

	extra := append([]otlploggrpc.Option{}, h.exporterOpts(t)...)
	extra = append(extra, otlploggrpc.WithRetry(otlploggrpc.RetryConfig{
		Enabled:         true,
		InitialInterval: 50 * time.Millisecond,
		MaxInterval:     200 * time.Millisecond,
		MaxElapsedTime:  5 * time.Second,
	}))

	exp, err := New(Config{
		Endpoint:             "127.0.0.1:1",
		Insecure:             true,
		BatchSize:            1,
		FlushInterval:        50 * time.Millisecond,
		ExportTimeout:        10 * time.Second,
		ExtraExporterOptions: extra,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = exp.Stop(ctx)
	})

	exp.Record(apikit.TelemetryEvent{Type: apikit.EventTypeAnalytics, SessionID: "abc"})

	if !waitFor(t, 5*time.Second, func() bool { return h.fake.calls.Load() >= 2 }) {
		t.Errorf("expected at least 2 attempts after Unavailable, got %d", h.fake.calls.Load())
	}
}

func TestExporter_NewRejectsEmptyEndpoint(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected error on empty endpoint")
	}
}

func TestFromEnv_MissingEndpoint(t *testing.T) {
	t.Setenv(EnvEndpoint, "")
	if _, err := FromEnv(); !errors.Is(err, ErrEndpointMissing) {
		t.Fatalf("expected ErrEndpointMissing, got %v", err)
	}
}

func TestFromEnv_ParsesAllFields(t *testing.T) {
	t.Setenv(EnvEndpoint, "collector.example:4317")
	t.Setenv("CLAWD_OTLP_GRPC_INSECURE", "true")
	t.Setenv("CLAWD_OTLP_GRPC_HEADERS", "x-auth=token,x-tenant=acme")
	t.Setenv("CLAWD_SERVICE_NAME", "svc")
	t.Setenv("CLAWD_SERVICE_VERSION", "v1")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.Endpoint != "collector.example:4317" {
		t.Errorf("Endpoint=%q", cfg.Endpoint)
	}
	if !cfg.Insecure {
		t.Error("Insecure not set")
	}
	if cfg.Headers["x-auth"] != "token" || cfg.Headers["x-tenant"] != "acme" {
		t.Errorf("Headers=%v", cfg.Headers)
	}
	if cfg.ServiceName != "svc" || cfg.ServiceVersion != "v1" {
		t.Errorf("Service name/version not parsed: %q %q", cfg.ServiceName, cfg.ServiceVersion)
	}
}

func TestFromEnv_HeadersIgnoresMalformed(t *testing.T) {
	t.Setenv(EnvEndpoint, "collector.example:4317")
	t.Setenv("CLAWD_OTLP_GRPC_HEADERS", "foo,bar=baz, =bad,good=value")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.Headers["bar"] != "baz" || cfg.Headers["good"] != "value" {
		t.Errorf("expected bar=baz and good=value, got %v", cfg.Headers)
	}
	if _, ok := cfg.Headers["foo"]; ok {
		t.Errorf("malformed entry foo without = should be skipped: %v", cfg.Headers)
	}
	if _, ok := cfg.Headers[""]; ok {
		t.Errorf("empty key should be skipped: %v", cfg.Headers)
	}
}

func TestEventToRecord_AttributesPopulated(t *testing.T) {
	ev := apikit.TelemetryEvent{
		Type:      apikit.EventTypeHTTPRequestFailed,
		SessionID: "sess",
		Method:    "POST",
		Path:      "/v1/messages",
		Error:     "rate_limited",
		Retryable: true,
	}
	rec := eventToRecord(ev)
	got := map[string]string{}
	rec.WalkAttributes(func(kv otellog.KeyValue) bool {
		got[kv.Key] = kv.Value.String()
		return true
	})

	for k, want := range map[string]string{
		"event.type":      "http_request_failed",
		"session.id":      "sess",
		"http.method":     "POST",
		"http.path":       "/v1/messages",
		"error.message":   "rate_limited",
		"error.retryable": "true",
	} {
		if got[k] != want {
			t.Errorf("attr %s: got %q want %q", k, got[k], want)
		}
	}
	if rec.SeverityText() != "ERROR" {
		t.Errorf("severity text: got %q want ERROR", rec.SeverityText())
	}
}

// ----- helpers -------------------------------------------------------------

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

func countRecords(reqs []*collogspb.ExportLogsServiceRequest) int {
	n := 0
	for _, req := range reqs {
		for _, rl := range req.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				n += len(sl.LogRecords)
			}
		}
	}
	return n
}

func hasResourceAttr(req *collogspb.ExportLogsServiceRequest, key, want string) bool {
	for _, rl := range req.ResourceLogs {
		if rl.Resource == nil {
			continue
		}
		for _, kv := range rl.Resource.Attributes {
			if kv.Key == key && kv.Value != nil && kv.Value.GetStringValue() == want {
				return true
			}
		}
	}
	return false
}
