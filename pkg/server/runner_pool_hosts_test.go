package server

import (
	"context"
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestAdminPutRunnerPools_ValidatesTheGatewayHost(t *testing.T) {
	s, _, done := newPoolMappingTestServer(t)
	defer done()
	registry := platformcfg.NewMemoryStore[platformcfg.RunnerPools]()
	s.runnerPoolsStore = registry
	call := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleAdminPutRunnerPools(w, orgReq(superAdminCtx(), http.MethodPut, "/api/admin/runner-pools", body, ""))
		return w
	}
	for _, body := range []string{`{"pools":[{"name":"alpha","gateway_base_url":"https://gw:"}]}`, `{"pools":[{"name":"alpha","gateway_base_url":"https://user@gw"}]}`, `{"pools":[{"name":"alpha","forge_hosts":["https://forge"]}]}`} {
		w := call(body)
		if w.Code != 422 || (!strings.Contains(w.Body.String(), "gateway") && !strings.Contains(w.Body.String(), "forge_hosts")) {
			t.Fatalf("invalid registry: %d %s", w.Code, w.Body.String())
		}
	}
	body := `{"pools":[{"name":"alpha","gateway_base_url":"https://gw.team.svc.cluster.local./v1","forge_hosts":["forge.internal:443"]}]}`
	for range 2 {
		w := call(body)
		if w.Code != 200 {
			t.Fatalf("private projection refused: %d %s", w.Code, w.Body.String())
		}
		var response struct {
			Pools    []platformcfg.RunnerPool      `json:"pools"`
			Warnings []platformcfg.PoolHostWarning `json:"warnings"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if len(response.Warnings) != 2 || response.Warnings[0].Code != platformcfg.PoolGatewayProjectionWarningCode {
			t.Fatalf("private projection lacks warnings: %s", w.Body.String())
		}
		stored, err := registry.Get(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(response.Pools, stored.Pools) || stored.Pools[0].GatewayBaseURL != "https://gw.team.svc.cluster.local./v1" {
			t.Fatal("validation or response rewrote registry")
		}
		raw, err := json.Marshal(stored)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "warnings") {
			t.Fatal("derived warning persisted")
		}
		get := httptest.NewRecorder()
		s.handleAdminGetRunnerPools(get, orgReq(superAdminCtx(), http.MethodGet, "/api/admin/runner-pools", "", ""))
		if !strings.Contains(get.Body.String(), `"warnings"`) {
			t.Fatalf("GET omitted warnings: %s", get.Body.String())
		}
	}
}
