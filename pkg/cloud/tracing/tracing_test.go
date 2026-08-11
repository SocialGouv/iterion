package tracing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"go.opentelemetry.io/otel"
	tracesdk "go.opentelemetry.io/otel/sdk/trace"
)

func TestInit_noEndpoint_isNoOp(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")

	shutdown, err := Init(context.Background(), "iterion-test", nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Init must always return a non-nil shutdown")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

func TestParseRatio(t *testing.T) {
	cases := []struct {
		in       string
		fallback float64
		want     float64
	}{
		{"", 0.5, 0.5},
		{"0.1", 1, 0.1},
		{"1", 1, 1},
		{"2", 1, 1},
		{"-0.5", 1, 0},
		{"not-a-float", 0.42, 0.42},
	}
	for _, tc := range cases {
		if got := parseRatio(tc.in, tc.fallback); got != tc.want {
			t.Errorf("parseRatio(%q, %v) = %v, want %v", tc.in, tc.fallback, got, tc.want)
		}
	}
}

func TestEnvSampler_default(t *testing.T) {
	t.Setenv("OTEL_TRACES_SAMPLER", "")
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "")
	if s := envSampler(); s == nil {
		t.Fatal("envSampler must always return a sampler")
	}
}

func TestEnvSampler_alwaysOff(t *testing.T) {
	t.Setenv("OTEL_TRACES_SAMPLER", "always_off")
	if got, want := envSampler().Description(), tracesdk.NeverSample().Description(); got != want {
		t.Errorf("Description = %q, want %q", got, want)
	}
}

func TestEnvSampler_ratio(t *testing.T) {
	t.Setenv("OTEL_TRACES_SAMPLER", "traceidratio")
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "0.25")
	s := envSampler()
	if s == nil {
		t.Fatal("nil sampler")
	}
	// Description carries the ratio so the operator can confirm the
	// config landed.
	if got := s.Description(); got == "" {
		t.Error("expected non-empty sampler description")
	}
}

func TestEnvSampler_unknown_fallsBackToParentBasedAlwaysOn(t *testing.T) {
	t.Setenv("OTEL_TRACES_SAMPLER", "no-such-sampler")
	s := envSampler()
	want := tracesdk.ParentBased(tracesdk.AlwaysSample()).Description()
	if got := s.Description(); got != want {
		t.Errorf("unknown sampler: Description = %q, want %q", got, want)
	}
}

func TestInit_withFakeEndpoint_buildsTracerProvider(t *testing.T) {
	// Fake OTLP collector that accepts everything. The exporter is
	// lazy on the wire so Init() succeeds even when the collector
	// never receives a span before shutdown.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", srv.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	shutdown, err := Init(context.Background(), "iterion-test", nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Init must return a non-nil shutdown")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

// TestInit_exportPath asserts the path the exporter actually PUTS ON THE WIRE
// for each endpoint shape the OTLP spec defines. That path is the only
// observable that moves: every one of these configurations compiles, connects,
// and looks healthy while silently POSTing spans somewhere no collector serves.
//
// The spec gives the two env vars DIFFERENT semantics
// (https://opentelemetry.io/docs/specs/otel/protocol/exporter/):
// OTEL_EXPORTER_OTLP_ENDPOINT is a base URL and the signal path is joined onto
// it; OTEL_EXPORTER_OTLP_TRACES_ENDPOINT is used as-is, with the root path when
// it carries none. The SDK's env reader implements both. Init must therefore
// NOT hand the resolved endpoint back through WithEndpointURL — explicit
// options are applied after the env config and would flatten the two readings
// into one.
//
// That flattening is what the otel 1.44 → 1.45 bump turned into span loss:
// 1.44 left a path-less URL's URLPath empty for cleanPath to fill with the
// default, 1.45 pins it to "/". Case 1 below is that regression.
func TestInit_exportPath(t *testing.T) {
	for _, tc := range []struct {
		name     string
		base     string // OTEL_EXPORTER_OTLP_ENDPOINT, "" = unset
		signal   string // OTEL_EXPORTER_OTLP_TRACES_ENDPOINT, "" = unset
		suffix   string // appended to the test server URL
		insecure bool   // OTEL_EXPORTER_OTLP_INSECURE, for the scheme-less shapes
		wantPath string
	}{
		// The #400 regression, and the shape most operators write.
		{name: "base endpoint, no path", base: "{srv}", wantPath: "/v1/traces"},
		// A collector behind a reverse-proxy prefix. The signal path is
		// joined onto the operator's prefix — dropping it POSTs to the
		// prefix itself, which is the same silent loss one level over.
		{name: "base endpoint with a prefix", base: "{srv}", suffix: "/otlp", wantPath: "/otlp/v1/traces"},
		// Per-signal: used as-is. No path means the ROOT path, not the
		// default signal path — "as-is" is the whole point of the variable.
		{name: "per-signal endpoint, no path", signal: "{srv}", wantPath: "/"},
		{name: "per-signal endpoint with a path", signal: "{srv}", suffix: "/v1/traces", wantPath: "/v1/traces"},
		// Precedence: the per-signal variable wins over the base one.
		{name: "per-signal wins over base", base: "http://127.0.0.1:1/wrong", signal: "{srv}", suffix: "/chosen", wantPath: "/chosen"},
		// The scheme-less `host:port` shape. url.Parse gives it no Host, so
		// the env reader resolves an EMPTY endpoint and WithEndpoint is what
		// makes the shape work at all — but WithEndpoint sets only the host.
		// For the per-signal variable the env reader has meanwhile applied its
		// "no path part means the root path" rule to a value whose path it
		// could not see, pinning URLPath to "/"; cleanPath will not replace a
		// non-empty path. Left alone this lands on the collector root — the
		// same silent loss, one shape over. There is no path to preserve here
		// (everything after the colon is opaque to url.Parse), so the signal
		// path is restated explicitly.
		//
		// These two run insecure: any scheme-less value parses as a scheme the
		// SDK does not recognise, which it treats as TLS. That is the SDK's
		// documented default for WithEndpoint and not what is under test, so
		// the spec's own INSECURE toggle takes it out of the way.
		{name: "base endpoint, host:port", base: "{host}", insecure: true, wantPath: "/v1/traces"},
		{name: "per-signal endpoint, host:port", signal: "{host}", insecure: true, wantPath: "/v1/traces"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths := make(chan string, 4)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case paths <- r.URL.Path:
				default:
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			subst := func(v string) string {
				switch v {
				case "{srv}":
					return srv.URL + tc.suffix
				case "{host}":
					// A NAME, not the listener's IP: url.Parse rejects
					// "127.0.0.1:4318" outright ("first path segment cannot
					// contain colon"), which makes the env reader skip and
					// hides the very behaviour under test. "localhost:4318"
					// parses — as scheme "localhost" — and is the shape an
					// operator actually writes.
					u, err := url.Parse(srv.URL)
					if err != nil {
						t.Fatalf("parse test server URL: %v", err)
					}
					return "localhost:" + u.Port() + tc.suffix
				}
				return v
			}
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", subst(tc.base))
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", subst(tc.signal))
			if tc.insecure {
				t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
			}

			ctx := context.Background()
			shutdown, err := Init(ctx, "iterion-test", nil)
			if err != nil {
				t.Fatalf("Init: %v", err)
			}
			_, span := otel.Tracer("tracing-test").Start(ctx, "probe")
			span.End()
			// Shutdown flushes the batcher, so the export completes before
			// we read — no polling, no sleep, no flake.
			if err := shutdown(ctx); err != nil {
				t.Fatalf("shutdown: %v", err)
			}

			select {
			case got := <-paths:
				if got != tc.wantPath {
					t.Errorf("exporter POSTed to %q, want %q — spans go to a path the collector does not serve", got, tc.wantPath)
				}
			default:
				t.Fatal("the collector received no export at all")
			}
		})
	}
}

func TestInit_endpointWithoutScheme_buildsTracerProvider(t *testing.T) {
	// "host:port" form takes the WithEndpoint branch, not WithEndpointURL.
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "localhost:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	shutdown, err := Init(context.Background(), "iterion-test", nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()
}
