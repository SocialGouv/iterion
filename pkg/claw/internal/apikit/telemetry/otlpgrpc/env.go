package otlpgrpc

import (
	"errors"
	"os"
	"strings"
)

// ErrEndpointMissing is returned by FromEnv when CLAWD_OTLP_GRPC_ENDPOINT
// is unset or blank. Callers treat this as "OTLP/gRPC export disabled" and
// skip wiring the sink.
var ErrEndpointMissing = errors.New("otlpgrpc: CLAWD_OTLP_GRPC_ENDPOINT not set")

// FromEnv builds a Config from environment variables. Returns
// ErrEndpointMissing when no endpoint is configured so callers can
// distinguish "user opted out" from "config invalid".
//
// Recognised vars:
//
//	CLAWD_OTLP_GRPC_ENDPOINT   Required. host:port or URL.
//	CLAWD_OTLP_GRPC_INSECURE   Optional. "1" / "true" disables TLS.
//	CLAWD_OTLP_GRPC_HEADERS    Optional. Comma-separated key=value pairs.
//	CLAWD_SERVICE_NAME         Optional. service.name resource attr.
//	CLAWD_SERVICE_VERSION      Optional. service.version resource attr.
func FromEnv() (Config, error) {
	endpoint := strings.TrimSpace(os.Getenv(EnvEndpoint))
	if endpoint == "" {
		return Config{}, ErrEndpointMissing
	}

	cfg := Config{
		Endpoint:       endpoint,
		ServiceName:    os.Getenv("CLAWD_SERVICE_NAME"),
		ServiceVersion: os.Getenv("CLAWD_SERVICE_VERSION"),
		Insecure:       parseBoolEnv(os.Getenv("CLAWD_OTLP_GRPC_INSECURE")),
		Headers:        parseHeaders(os.Getenv("CLAWD_OTLP_GRPC_HEADERS")),
	}
	return cfg, nil
}

func parseBoolEnv(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func parseHeaders(s string) map[string]string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	out := make(map[string]string)
	for _, pair := range strings.Split(s, ",") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.TrimSpace(kv[0])
		v := strings.TrimSpace(kv[1])
		if k == "" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
