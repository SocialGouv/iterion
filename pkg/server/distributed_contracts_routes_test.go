package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/portsactivation/authority"
)

func TestDistributedContractsRoutesAreOptInAndOperatorProtected(t *testing.T) {
	off := New(Config{DisableAuth: true, SkipProjectRegistration: true}, iterlog.New(iterlog.LevelError, nil))
	request := httptest.NewRequest(http.MethodGet, "/api/admin/contracts/distributed", nil)
	response := httptest.NewRecorder()
	off.mux.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("disabled distributed route status = %d, want 404", response.Code)
	}

	protected := New(Config{DistributedAuthority: &authority.Authority{}, SkipProjectRegistration: true}, iterlog.New(iterlog.LevelError, nil))
	request = httptest.NewRequest(http.MethodGet, "/api/admin/contracts/distributed", nil)
	response = httptest.NewRecorder()
	protected.mux.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated distributed status = %d, want 401", response.Code)
	}
}

func TestDistributedContractsHandlersRejectCallerProofAndUnknownFields(t *testing.T) {
	srv := New(Config{DisableAuth: true, DistributedAuthority: &authority.Authority{}, SkipProjectRegistration: true}, iterlog.New(iterlog.LevelError, nil))
	for _, test := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/admin/contracts/distributed/probe", `{"proof":"caller"}`},
		{http.MethodPost, "/api/admin/contracts/distributed/probe", `null`},
		{http.MethodPost, "/api/admin/contracts/distributed/activate", `{"expected_policy_revision":0,"proof":"caller"}`},
		{http.MethodPost, "/api/admin/contracts/distributed/activate", `{"unknown":1}`},
		{http.MethodPost, "/api/admin/contracts/distributed/activate", `{}`},
		{http.MethodPost, "/api/admin/contracts/distributed/activate", `{"expected_policy_revision":null}`},
	} {
		t.Run(test.path+test.body, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, bytes.NewBufferString(test.body))
			response := httptest.NewRecorder()
			srv.mux.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("request status = %d, body=%s; want 400", response.Code, response.Body.String())
			}
		})
	}
	// The handlers must not reach a nil persistence authority for an empty
	// request: decoding is the boundary that rejects malformed input first.
	request := httptest.NewRequest(http.MethodPost, "/api/admin/contracts/distributed/probe", bytes.NewBufferString(`{}`))
	response := httptest.NewRecorder()
	srv.mux.ServeHTTP(response, request)
	if response.Code == http.StatusBadRequest {
		t.Fatal("empty probe body was rejected by the request contract")
	}
}
