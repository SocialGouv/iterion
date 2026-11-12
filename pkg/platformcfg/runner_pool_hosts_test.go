package platformcfg

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestPoolHostProjectionShape(t *testing.T) {
	for _, raw := range []string{"https://gateway.example/v1", "HTTP://Gateway.Example.:80/v1", "http://gateway.namespace.svc.cluster.local./v1", "https://10.2.3.4:8443", "https://[::1]/v1", "http://[::ffff:127.0.0.1]:8080", "https://école.example/v1"} {
		p := RunnerPool{Name: "alpha", GatewayBaseURL: raw, ForgeHosts: []string{"github.com", "forge.svc:443", "[fd00::1]:8443"}}
		before := p
		r := RunnerPools{Pools: []RunnerPool{p}}
		for range 2 {
			if err := r.Validate(); err != nil {
				t.Fatalf("valid %q: %v", raw, err)
			}
		}
		if !reflect.DeepEqual(r.Pools[0], before) {
			t.Fatalf("validation rewrote explicit URL %q", raw)
		}
	}
	for _, raw := range []string{"https://", "mailto:gateway", "ftp://gateway", "https://user:password@gateway", "https://gateway?", "https://gateway#", "https://gateway/path?a=b", "https://gateway/#fragment", " https://gateway", "https://gate way", "https://gateway:", "https://gateway:0", "https://gateway:65536", "https://gateway:99999", "https://[bad]/", "https://[::1]:", "https://a..example", "https://-bad.example", "https://a.example..", "https://%zz"} {
		r := RunnerPools{Pools: []RunnerPool{{Name: "alpha", GatewayBaseURL: raw}}}
		if err := r.Validate(); err == nil {
			t.Errorf("accepted invalid URL %q", raw)
		}
	}
	for _, host := range []string{"", "https://forge.example", "forge/path", "user@forge", "forge?query", "forge:", "forge:70000", "::1", "forge bad", ".forge", "forge.."} {
		r := RunnerPools{Pools: []RunnerPool{{Name: "alpha", ForgeHosts: []string{host}}}}
		if err := r.Validate(); err == nil {
			t.Errorf("accepted invalid forge host %q", host)
		}
	}
}

func TestPoolHostProjectionWarnsWithoutRefusing(t *testing.T) {
	for _, host := range []string{"localhost", "localhost.", "gateway", "gateway.namespace.svc.cluster.local.", "gateway.internal", "127.0.0.1", "10.0.0.4", "169.254.169.254", "[::1]", "[::ffff:127.0.0.1]", "[fd00::1]"} {
		r := RunnerPools{Pools: []RunnerPool{{Name: "alpha", GatewayBaseURL: "https://" + host}}}
		if err := r.Validate(); err != nil {
			t.Fatalf("private host refused %q: %v", host, err)
		}
		warnings := r.HostWarnings()
		if len(warnings) != 1 || warnings[0].Pool != "alpha" || warnings[0].Field != "gateway_base_url" || warnings[0].Code != PoolGatewayProjectionWarningCode {
			t.Fatalf("private host %q not named: %+v", host, warnings)
		}
		if !reflect.DeepEqual(warnings, r.HostWarnings()) {
			t.Fatal("warning derivation not idempotent")
		}
	}
	r := RunnerPools{Pools: []RunnerPool{{Name: "alpha", GatewayBaseURL: "https://gateway.example", ForgeHosts: []string{"github.com"}}}}
	if warnings := r.HostWarnings(); len(warnings) != 0 {
		t.Fatalf("public hosts warned: %+v", warnings)
	}
}

type poolBootReader struct {
	value   *RunnerPools
	err     error
	calls   int
	bounded bool
}

func (r *poolBootReader) Get(ctx context.Context) (*RunnerPools, error) {
	r.calls++
	_, r.bounded = ctx.Deadline()
	return r.value, r.err
}

func TestPoolBootWarnsWhenItsGatewayDisagreesWithTheRegistry(t *testing.T) {
	for _, tc := range []struct {
		name, pool, declared, actual, contains string
		readErr                                error
		count                                  int
	}{
		{name: "same", pool: "alpha", declared: "https://GW.example.:443/v1", actual: "https://gw.example/other"},
		{name: "http-default", pool: "alpha", declared: "http://gw:80", actual: "http://GW./v1"},
		{name: "different-host", pool: "alpha", declared: "https://gw.example", actual: "https://other.example", contains: "differs", count: 1},
		{name: "different-port", pool: "alpha", declared: "https://gw.example:8443", actual: "https://gw.example", contains: "differs", count: 1},
		{name: "read-error", pool: "alpha", readErr: errors.New("registry unavailable"), contains: "registry unavailable", count: 1},
		{name: "missing", pool: "alpha", contains: "missing", count: 1},
		{name: "invalid-effective", pool: "alpha", declared: "https://gw", actual: "https://gw:", contains: "effective gateway", count: 1},
		{name: "invalid-stored", pool: "alpha", declared: "https://gw:99999", actual: "https://gw", contains: "stored gateway", count: 1},
		{name: "shared", declared: "https://gw", actual: "https://other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &poolBootReader{value: &RunnerPools{Pools: []RunnerPool{{Name: "alpha", GatewayBaseURL: tc.declared}}}, err: tc.readErr}
			var messages []string
			CheckPoolGatewayAtBoot(context.Background(), reader, tc.pool, tc.actual, func(f string, a ...any) { messages = append(messages, fmt.Sprintf(f, a...)) })
			if len(messages) != tc.count {
				t.Fatalf("boot warnings=%v want%d", messages, tc.count)
			}
			if tc.count > 0 && (!strings.Contains(messages[0], tc.contains) || !strings.Contains(messages[0], PoolGatewayProjectionWarningCode)) {
				t.Fatalf("warning lost reason/code: %v", messages)
			}
			if tc.pool != "" && (reader.calls != 1 || !reader.bounded) {
				t.Fatalf("registry read calls=%d bounded=%v", reader.calls, reader.bounded)
			}
			if tc.pool == "" && reader.calls != 0 {
				t.Fatal("shared boot read pool registry")
			}
		})
	}
}
