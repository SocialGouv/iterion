package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/SocialGouv/iterion/pkg/store"
)

// Distributed native activation is intentionally a very small operator API.
// The server owns the privileged probe and the proof write; callers can only
// select the policy revision they observed before asking for activation.
func (s *Server) registerDistributedContractsRoutes() {
	if s.cfg.DistributedAuthority == nil {
		return
	}
	s.mux.Handle("GET /api/admin/contracts/distributed", s.requireSuperAdmin(http.HandlerFunc(s.handleDistributedContractsStatus)))
	s.mux.Handle("POST /api/admin/contracts/distributed/probe", s.requireSuperAdmin(http.HandlerFunc(s.handleDistributedContractsProbe)))
	s.mux.Handle("POST /api/admin/contracts/distributed/activate", s.requireSuperAdmin(http.HandlerFunc(s.handleDistributedContractsActivate)))
}

type distributedContractsStatus struct {
	Activation *store.PortActivation    `json:"activation,omitempty"`
	Candidate  *distributedProofSummary `json:"candidate,omitempty"`
}

type distributedProofSummary struct {
	PolicyRevision    uint64 `json:"policy_revision"`
	ProofRevision     uint64 `json:"proof_revision"`
	AuthorityEpoch    uint64 `json:"authority_epoch"`
	StoreIdentity     string `json:"store_identity"`
	ProofDigest       string `json:"proof_digest"`
	ObservationDigest string `json:"observation_digest"`
	VerifiedAt        string `json:"verified_at"`
	ExpiresAt         string `json:"expires_at"`
}

func summarizeDistributedProof(proof *store.PortDistributedProof) *distributedProofSummary {
	if proof == nil {
		return nil
	}
	return &distributedProofSummary{
		PolicyRevision: proof.PolicyRevision, ProofRevision: proof.ProofRevision,
		AuthorityEpoch: proof.AuthorityEpoch, StoreIdentity: proof.StoreIdentity,
		ProofDigest: proof.ProofDigest, ObservationDigest: proof.ObservationDigest,
		VerifiedAt: proof.VerifiedAt.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		ExpiresAt:  proof.ExpiresAt.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
	}
}

func (s *Server) handleDistributedContractsStatus(w http.ResponseWriter, r *http.Request) {
	authority := s.cfg.DistributedAuthority
	if authority == nil {
		httpError(w, http.StatusNotFound, "distributed contracts authority is disabled")
		return
	}
	if authority.Activations == nil {
		httpError(w, http.StatusServiceUnavailable, "distributed contracts authority is not configured")
		return
	}
	activation, err := authority.Activations.LoadPortActivation(r.Context())
	if err != nil {
		httpError(w, http.StatusServiceUnavailable, "read distributed activation: %s", err.Error())
		return
	}
	var candidate *store.PortDistributedProof
	if authority.Candidates != nil {
		candidate, err = authority.Candidates.LoadPortDistributedProofCandidate(r.Context())
		if err != nil {
			httpError(w, http.StatusServiceUnavailable, "read distributed probe candidate: %s", err.Error())
			return
		}
	}
	writeJSON(w, distributedContractsStatus{Activation: activation, Candidate: summarizeDistributedProof(candidate)})
}

func (s *Server) handleDistributedContractsProbe(w http.ResponseWriter, r *http.Request) {
	authority := s.cfg.DistributedAuthority
	if authority == nil {
		httpError(w, http.StatusNotFound, "distributed contracts authority is disabled")
		return
	}
	if err := decodeDistributedContractsBody(w, r, &struct{}{}, true); err != nil {
		return
	}
	proof, err := authority.Probe(r.Context())
	if err != nil {
		writeDistributedContractsError(w, err)
		return
	}
	// The snapshot is already a safe, digest-bound projection. Returning it to
	// the authenticated operator makes the probe reviewable without exposing
	// the Secret source bytes or accepting a proof back from the caller.
	writeJSON(w, proof)
}

type distributedContractsActivateRequest struct {
	ExpectedPolicyRevision *uint64 `json:"expected_policy_revision"`
}

func (s *Server) handleDistributedContractsActivate(w http.ResponseWriter, r *http.Request) {
	authority := s.cfg.DistributedAuthority
	if authority == nil {
		httpError(w, http.StatusNotFound, "distributed contracts authority is disabled")
		return
	}
	var request distributedContractsActivateRequest
	if err := decodeDistributedContractsBody(w, r, &request, false); err != nil {
		return
	}
	if request.ExpectedPolicyRevision == nil {
		httpError(w, http.StatusBadRequest, "expected_policy_revision is required")
		return
	}
	record, err := authority.Activate(r.Context(), *request.ExpectedPolicyRevision)
	if err != nil {
		writeDistributedContractsError(w, err)
		return
	}
	writeJSON(w, record)
}

func decodeDistributedContractsBody(w http.ResponseWriter, r *http.Request, target any, optionalEmpty bool) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		if optionalEmpty && errors.Is(err, io.EOF) {
			return nil
		}
		httpError(w, http.StatusBadRequest, "invalid distributed contracts request: %s", err.Error())
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		httpError(w, http.StatusBadRequest, "distributed contracts request must contain one JSON value")
		return errors.New("trailing distributed contracts request data")
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		err := errors.New("null distributed contracts request is not allowed")
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return err
	}
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	if err := strict.Decode(target); err != nil {
		httpError(w, http.StatusBadRequest, "invalid distributed contracts request: %s", err.Error())
		return err
	}
	if err := strict.Decode(new(any)); !errors.Is(err, io.EOF) {
		httpError(w, http.StatusBadRequest, "distributed contracts request must contain one JSON value")
		return errors.New("trailing distributed contracts request data")
	}
	return nil
}

func writeDistributedContractsError(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	switch {
	case errors.Is(err, store.ErrRunConflict):
		status = http.StatusConflict
	case errors.Is(err, store.ErrPortActivation):
		status = http.StatusUnprocessableEntity
	}
	httpError(w, status, "%s", err.Error())
}
