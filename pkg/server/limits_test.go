package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// readCostCap GETs the cost-cap status and decodes it.
func readCostCap(t *testing.T, base string) map[string]any {
	t.Helper()
	resp, err := http.Get(base + "/api/v1/limits/cost")
	if err != nil {
		t.Fatalf("GET limits/cost: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET limits/cost status = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func TestCostCapStatusDisabledByDefault(t *testing.T) {
	_, hs := newTestServer(t)
	out := readCostCap(t, hs.URL)
	if enabled, _ := out["enabled"].(bool); enabled {
		t.Errorf("cost cap should be disabled by default, got %+v", out)
	}
}

func TestCostCapStatusAndOverride(t *testing.T) {
	// Enable the cap via env before the server builds its run service.
	t.Setenv("ITERION_MAX_COST_PER_DAY_USD", "5")
	srv, hs := newTestServer(t)
	if srv.runs.DailyCap() == nil {
		t.Fatal("expected daily cap to be enabled via env")
	}

	// server_info should advertise the cap.
	infoResp, err := http.Get(hs.URL + "/api/server/info")
	if err != nil {
		t.Fatalf("GET server/info: %v", err)
	}
	var info map[string]any
	_ = json.NewDecoder(infoResp.Body).Decode(&info)
	infoResp.Body.Close()
	if enabled, _ := info["cost_cap_enabled"].(bool); !enabled {
		t.Errorf("server_info cost_cap_enabled = false, want true")
	}

	// Status reports enabled with the configured limit.
	out := readCostCap(t, hs.URL)
	if enabled, _ := out["enabled"].(bool); !enabled {
		t.Fatalf("cost cap should be enabled, got %+v", out)
	}
	if lim, _ := out["limit_usd"].(float64); lim != 5 {
		t.Errorf("limit_usd = %v, want 5", lim)
	}

	// Override for today.
	resp, err := http.Post(hs.URL+"/api/v1/limits/cost/override", "application/json",
		strings.NewReader(`{"note":"ship it"}`))
	if err != nil {
		t.Fatalf("POST override: %v", err)
	}
	var ovOut map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&ovOut)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("override status = %d, want 200", resp.StatusCode)
	}
	if active, _ := ovOut["override_active"].(bool); !active {
		t.Errorf("override_active = false after override, want true: %+v", ovOut)
	}

	// Re-reading status confirms the override persisted.
	out = readCostCap(t, hs.URL)
	if active, _ := out["override_active"].(bool); !active {
		t.Errorf("override not persisted: %+v", out)
	}
}
