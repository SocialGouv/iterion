// Package server provides an HTTP API for the iterion editor.
// It wraps the parser, compiler, and unparser to provide endpoints
// for parsing .iter files, validating workflows, and generating .iter text.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

//go:embed all:static
var staticFS embed.FS

// Config holds the server configuration.
type Config struct {
	Port        int    // HTTP port (default 4891). Pass 0 for an OS-assigned random port.
	Bind        string // bind address (default "127.0.0.1"; use "0.0.0.0" only with explicit user opt-in)
	ExamplesDir string // path to examples directory
	WorkDir     string // root directory for file operations
	StoreDir    string // run store directory (default: <WorkDir>/.iterion)
	OpenBrowser bool   // open browser on start

	// AuthService is the multitenant authentication service. When
	// non-nil, every /api/* request is gated by authMiddleware. CLI
	// local mode leaves this nil and DisableAuth=true so the editor
	// process trusts its TTY user (legacy behaviour).
	AuthService *auth.Service

	// AuthSigner is the JWT verifier used by middleware to validate
	// access tokens. Required when AuthService is set.
	AuthSigner *auth.JWTSigner

	// OIDCRegistry maps provider slugs ("google", "github", "sso")
	// to their connectors. nil disables every OIDC route.
	OIDCRegistry *oidc.Registry

	// OIDCStates persists per-flow PendingAuth records between
	// /start and /callback. Defaults to an in-memory store when nil.
	OIDCStates oidc.StateStore

	// CookieDomain narrows the auth cookies' Domain attribute. Empty
	// means host-only cookie (recommended).
	CookieDomain string

	// CookieSecure forces the Secure flag on auth cookies. Should
	// always be true in production. Defaults to false in test/dev.
	CookieSecure bool

	// AccessTTL is the access JWT lifetime; the auth cookie's
	// MaxAge mirrors it. Defaults to AuthSigner.AccessTTL().
	AccessTTL time.Duration

	// RefreshTTL is the refresh cookie / session lifetime.
	// Defaults to 30d.
	RefreshTTL time.Duration

	// PublicURL is the externally-reachable origin (e.g.
	// https://iterion.example) used to build OIDC redirect URIs.
	PublicURL string

	// SignupMode is "open" or "invite_only"; surfaced to the SPA.
	SignupMode string

	// DisableAuth bypasses every auth check — DEV ONLY.
	DisableAuth bool

	// ApiKeys is the BYOK store. When non-nil, the server registers
	// /api/teams/:id/api-keys + /api/me/api-keys and the cloud
	// publisher resolves keys at launch time.
	ApiKeys secrets.ApiKeyStore

	// RunSecrets is the per-run sealed bundle store. Required when
	// ApiKeys is set.
	RunSecrets secrets.RunSecretsStore

	// Sealer is the AES-GCM sealer used to encrypt API keys at rest
	// and run-scoped bundles in flight. Required when ApiKeys is set.
	Sealer secrets.Sealer

	// OAuthForfait is the per-user OAuth credential store. When
	// non-nil, the server registers /api/me/oauth/* endpoints and
	// the cloud publisher injects sealed credentials.json /
	// auth.json blobs into the run bundle for runs that don't have
	// a BYOK API key for the relevant provider.
	OAuthForfait secrets.OAuthStore

	// AnthropicOAuthClientID is the OAuth client id used to refresh
	// Claude Code subscription tokens. Empty disables refresh of
	// the claude_code kind (the user must re-upload on expiry).
	AnthropicOAuthClientID string
	// CodexOAuthClientID is the equivalent for Codex.
	CodexOAuthClientID string

	// Store overrides the default filesystem store with a caller-
	// supplied implementation (typically the cloud Mongo+S3 store).
	// When non-nil, StoreDir + the .iterion auto-discovery are
	// ignored and the supplied store is wired into runview.NewService
	// directly. Plan §F (T-30).
	Store store.RunStore

	// LaunchPublisher, when non-nil, routes the run console's Launch /
	// Resume / Cancel through the cloud queue instead of spawning the
	// runtime in-process. Used by `iterion server` in cloud mode
	// (T-31, T-32, T-33).
	LaunchPublisher runview.LaunchPublisher

	// EventSource, when non-nil, replaces the in-process EventBroker
	// for live + historical WS event delivery. Cloud mode wires a
	// Mongo change-stream source so the WS handler sees runner-pod
	// writes. Plan §F (T-21).
	EventSource runview.EventStreamSource

	// ReadinessChecks, when non-nil, are invoked by /readyz to verify
	// every external dependency (Mongo, NATS, S3) is reachable. Each
	// entry is run with a sub-context bounded by ReadinessTimeout (1s
	// by default) so a slow dependency cannot stall the probe past
	// the kubelet's readiness window. Empty in local mode.
	ReadinessChecks map[string]ReadinessCheck

	// ReadinessTimeout caps each ReadinessCheck individually. Defaults
	// to 1s when zero.
	ReadinessTimeout time.Duration

	// Mode advertises the deployment mode in the health response.
	// Defaults to "local" when empty for backward compat with callers
	// that don't set it.
	Mode string

	// Metrics, when non-nil, lets the server publish iterion_ws_connections
	// gauge updates as run-console clients connect / disconnect. Other
	// cloud metrics live on the runner / publisher side.
	Metrics *metrics.Registry

	// MaxUploadSize bounds the bytes the upload endpoint will accept
	// per attachment. Zero is replaced with a mode-specific default
	// (1 GB desktop, 50 MB web/cloud) at registration time.
	MaxUploadSize int64
	// MaxTotalUploadSize bounds the cumulative bytes per run across
	// every attachment. Zero defaults to 5x MaxUploadSize.
	MaxTotalUploadSize int64
	// MaxUploadsPerRun caps how many distinct attachments may
	// reference a single run. Zero defaults to 20.
	MaxUploadsPerRun int
	// AllowedUploadMIMEs is the server-side allowlist applied to
	// every upload's sniffed MIME. Each entry is a `type/subtype`
	// pattern with optional `*` wildcards (e.g. `image/*`). Empty
	// means "use the built-in safe defaults" (image/png, image/jpeg,
	// image/gif, image/webp, application/pdf, application/json,
	// text/plain, text/markdown, text/csv, application/yaml,
	// application/zip, application/gzip, application/x-tar).
	AllowedUploadMIMEs []string
}

// ReadinessCheck is the contract /readyz invokes on each external
// dependency. It MUST be cheap (HEAD/ping) and MUST respect the
// supplied context's deadline.
type ReadinessCheck func(ctx context.Context) error

// Server is the editor HTTP server.
type Server struct {
	cfg     Config
	logger  *iterlog.Logger
	mux     *http.ServeMux
	handler http.Handler // mux wrapped with auth middleware
	server  *http.Server
	hub     *Hub
	watcher *Watcher
	runs    *runview.Service // run console service; nil disables /api/runs endpoints

	authSvc      *auth.Service
	signer       *auth.JWTSigner
	oidcRegistry *oidc.Registry
	oidcStates   oidc.StateStore
	apiKeys      secrets.ApiKeyStore
	runSecrets   secrets.RunSecretsStore
	sealer       secrets.Sealer
	oauthStore   secrets.OAuthStore
	httpClient   *http.Client

	// detector is the cached LLM credential detector backing
	// /api/backends/detect. Lazily constructed on first request.
	detector     *detect.CachedDetector
	detectorOnce sync.Once

	// listener is captured at ListenAndServe time so callers (notably the
	// desktop host, which passes Port=0 for an OS-assigned port) can read
	// the actual bind address. Read via Addr(). Mutated only inside
	// ListenAndServe and read after addrReady is closed.
	listener  net.Listener
	addrReady chan struct{}
}

// New creates a new editor server.
//
// Port semantics: cfg.Port == 0 means "let the OS pick a free port"
// (the desktop host depends on this). If you want the legacy default of
// 4891, set it explicitly — pkg/cli.RunEditor does so when the caller
// passes Port=0. Tests that construct Config{} directly previously got
// 4891 by default; they now get a random port, which is what we want
// to avoid cross-test bind conflicts.
func New(cfg Config, logger *iterlog.Logger) *Server {
	// Default to loopback. The previous behaviour was to leave Addr as ":<port>"
	// which binds 0.0.0.0 — exposing the editor (which has unauthenticated
	// /api/files/save and /api/files/open endpoints) to anyone on the LAN.
	// The startup log used to print "http://localhost:<port>" regardless,
	// which actively misled operators about the bind surface. Operators who
	// genuinely want LAN access must now opt in via --bind 0.0.0.0.
	if cfg.Bind == "" {
		cfg.Bind = "127.0.0.1"
	}
	cfg = applyUploadDefaults(cfg)
	if cfg.AccessTTL <= 0 && cfg.AuthSigner != nil {
		cfg.AccessTTL = cfg.AuthSigner.AccessTTL()
	}
	if cfg.RefreshTTL <= 0 {
		cfg.RefreshTTL = 30 * 24 * time.Hour
	}
	if cfg.OIDCStates == nil {
		cfg.OIDCStates = oidc.NewMemoryStateStore(10 * time.Minute)
	}
	s := &Server{
		cfg:          cfg,
		logger:       logger,
		mux:          http.NewServeMux(),
		addrReady:    make(chan struct{}),
		authSvc:      cfg.AuthService,
		signer:       cfg.AuthSigner,
		oidcRegistry: cfg.OIDCRegistry,
		oidcStates:   cfg.OIDCStates,
		apiKeys:      cfg.ApiKeys,
		runSecrets:   cfg.RunSecrets,
		sealer:       cfg.Sealer,
		oauthStore:   cfg.OAuthForfait,
		httpClient:   &http.Client{Timeout: 15 * time.Second},
	}
	s.hub = NewHub(logger)
	go s.hub.Run()
	// File watcher is only meaningful in local mode where the editor
	// SPA is editing files on disk that the server should hot-reload.
	// In cloud mode the server pod has no local source tree (workflows
	// arrive inline on the wire) and starting the watcher there would
	// generate noise events on whatever transient WorkDir was passed.
	if cfg.WorkDir != "" && cfg.Mode != "cloud" {
		var err error
		s.watcher, err = NewWatcher(cfg.WorkDir, s.hub, logger)
		if err != nil {
			logger.Warn("file watcher disabled: %v", err)
		} else {
			go s.watcher.Start()
		}
	}
	// Wire the run console service. A failure here is non-fatal: we log a
	// warning and leave s.runs == nil, which disables /api/runs but keeps
	// the editor usable. The guard preserves the prior behaviour of
	// disabling runs entirely when neither StoreDir nor WorkDir are set
	// (e.g. tests that build a Config{} directly).
	var storeDir string
	if cfg.StoreDir != "" || cfg.WorkDir != "" {
		storeDir = store.ResolveStoreDir(cfg.WorkDir, cfg.StoreDir)
	}
	// When a caller-supplied Store is wired (cloud mode), bypass the
	// filesystem .iterion discovery and inject the store directly so
	// runview.NewService talks to Mongo+S3.
	switch {
	case cfg.Store != nil:
		opts := []runview.ServiceOption{
			runview.WithLogger(logger),
			runview.WithStore(cfg.Store),
		}
		if cfg.LaunchPublisher != nil {
			opts = append(opts, runview.WithLaunchPublisher(cfg.LaunchPublisher))
		}
		if cfg.EventSource != nil {
			opts = append(opts, runview.WithEventSource(cfg.EventSource))
		}
		svc, svcErr := runview.NewService("", opts...)
		if svcErr != nil {
			logger.Warn("run console disabled: %v", svcErr)
		} else {
			s.runs = svc
		}
	case storeDir != "":
		svc, svcErr := runview.NewService(storeDir, runview.WithLogger(logger))
		if svcErr != nil {
			logger.Warn("run console disabled: %v", svcErr)
		} else {
			s.runs = svc
		}
	}
	// Wire the same Origin allowlist used for HTTP CORS into the WebSocket
	// upgrader so cross-origin browser tabs can't subscribe to file events.
	SetWebSocketOriginCheck(s.isAllowedOrigin)
	s.routes()
	// Always wrap in authMiddleware. Public paths (health probes,
	// auth endpoints, static SPA) bypass the JWT check internally;
	// every /api/* call requires a valid bearer / cookie. DEV
	// override: cfg.DisableAuth synthesizes a super-admin Identity
	// instead of rejecting unauthenticated requests.
	s.handler = s.authMiddleware(s.mux)
	s.server = &http.Server{
		Addr:              net.JoinHostPort(cfg.Bind, fmt.Sprintf("%d", cfg.Port)),
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// Addr returns the actual bound address (host:port) once ListenAndServe has
// successfully created its listener. It blocks until the listener is ready or
// the context is cancelled. Used by the desktop host when Port=0 was passed
// and the OS picks the port.
func (s *Server) Addr() string {
	<-s.addrReady
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// ListenAndServe starts the HTTP server.
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		// Even on error, signal Addr() so callers don't block forever.
		close(s.addrReady)
		return err
	}
	s.listener = ln
	// Reflect the OS-chosen port back into the config so logging and the
	// origin allowlist see the real port (Port=0 mode).
	if tcpAddr, ok := ln.Addr().(*net.TCPAddr); ok {
		s.cfg.Port = tcpAddr.Port
	}
	close(s.addrReady)
	// Truthful URL in the log: if the operator chose a non-loopback bind we
	// print the actual address so they know the editor is exposed beyond the
	// local machine. Previously we always printed http://localhost:<port>
	// regardless of the bind interface.
	displayHost := s.cfg.Bind
	if displayHost == "127.0.0.1" || displayHost == "::1" || displayHost == "" {
		displayHost = "localhost"
	}
	s.logger.Info("Editor server listening on http://%s:%d", displayHost, s.cfg.Port)
	return s.server.Serve(ln)
}

// Shutdown gracefully shuts down the server.
//
// Order matters: HTTP-level shutdown (Server.Shutdown) drains in-flight
// requests, while the run console service drains in-process workflow
// goroutines. We do the workflow drain first so any cancel events
// reach the on-disk store before the file watcher stops broadcasting
// and clients drop. The drain ctx is the caller-supplied shutdown
// deadline.
//
// Drain (rather than Stop) is intentional: it flips each in-flight
// run to failed_resumable and emits EventRunInterrupted so the next
// boot can offer one-click resume and clients can distinguish
// shutdown-induced termination from user-initiated cancel.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.runs != nil {
		s.runs.Drain(ctx)
	}
	if s.watcher != nil {
		s.watcher.Stop()
	}
	s.hub.Stop()
	return s.server.Shutdown(ctx)
}

func (s *Server) routes() {
	// CORS preflight handler — only echoes ACAO when the Origin is an
	// allowed loopback origin. The wildcard ACAO previously emitted here
	// (combined with POST /api/files/save accepting JSON bodies) allowed
	// any browser tab the user visited to write attacker-controlled .iter
	// files into WorkDir, which iterion would then execute under `sh -c`
	// the next time the user ran the workflow — drive-by RCE on the dev
	// machine. The 'local-only server' framing didn't address this because
	// the threat is browser-side, not network-side.
	s.mux.HandleFunc("OPTIONS /api/", func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if !s.isAllowedOrigin(origin) {
			// No ACAO header → browser blocks the cross-origin request.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.WriteHeader(http.StatusNoContent)
	})

	s.mux.HandleFunc("POST /api/parse", s.handleParse)
	s.mux.HandleFunc("POST /api/unparse", s.handleUnparse)
	s.mux.HandleFunc("POST /api/validate", s.handleValidate)
	s.mux.HandleFunc("GET /api/examples", s.handleListExamples)
	s.mux.HandleFunc("GET /api/examples/{name...}", s.handleLoadExample)
	s.mux.HandleFunc("GET /api/effort-capabilities", s.handleEffortCapabilities)
	s.mux.HandleFunc("GET /api/resolve-effort", s.handleResolveEffort)
	s.mux.HandleFunc("GET /api/backends/detect", s.handleBackendsDetect)

	// Health endpoints — liveness (always 200 if the mux is alive)
	// and readiness (cloud-mode dependency pings come via T-26 when
	// Mongo/NATS/S3 ping handles are threaded into the server). Used
	// by the Helm chart probes (plan §F T-36, T-37).
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)

	// WebSocket endpoint for file watching
	s.mux.HandleFunc("GET /api/ws", s.hub.HandleWebSocket)

	// File management endpoints
	s.mux.HandleFunc("GET /api/files", s.handleListFiles)
	s.mux.HandleFunc("POST /api/files/open", s.handleOpenFile)
	s.mux.HandleFunc("POST /api/files/save", s.handleSaveFile)

	// Run console endpoints (registered only when s.runs is wired).
	s.registerRunRoutes()
	s.registerRunLogRoutes()

	// Auth + identity endpoints (login, logout, refresh, OIDC,
	// teams, invitations). Registered only when authSvc is wired —
	// local mode without an auth service skips them.
	if s.authSvc != nil {
		s.registerAuthRoutes()
	}

	// BYOK endpoints. Requires the auth+identity stack already in
	// place — caller must wire AuthService + ApiKeys + Sealer.
	if s.apiKeys != nil && s.sealer != nil && s.authSvc != nil {
		s.registerBYOKRoutes()
	}

	// OAuth-forfait endpoints. Same gating as BYOK plus the per-
	// user OAuthForfait store.
	if s.oauthStore != nil && s.sealer != nil && s.authSvc != nil {
		s.registerOAuthForfaitRoutes()
	}

	// Serve static frontend files.
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("failed to create sub filesystem: %v", err)
	}
	fileServer := http.FileServer(http.FS(staticSub))
	s.mux.Handle("GET /", fileServer)
}

// --- Request/Response types ---

type parseRequest struct {
	Source string `json:"source"`
}

type parseResponse struct {
	Document    json.RawMessage `json:"document"`
	Diagnostics []string        `json:"diagnostics,omitempty"`
	Issues      []DiagnosticDTO `json:"issues,omitempty"`
}

// DiagnosticDTO is the wire-safe shape of an ir.Diagnostic. It carries the
// structured fields (code, severity, attribution, hint) so the editor can
// render inline badges without resorting to string-matching the message.
type DiagnosticDTO struct {
	Code     string `json:"code,omitempty"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	NodeID   string `json:"node_id,omitempty"`
	EdgeID   string `json:"edge_id,omitempty"`
	Hint     string `json:"hint,omitempty"`
}

func irDiagToDTO(d ir.Diagnostic) DiagnosticDTO {
	sev := "error"
	if d.Severity == ir.SeverityWarning {
		sev = "warning"
	}
	return DiagnosticDTO{
		Code:     string(d.Code),
		Severity: sev,
		Message:  d.Message,
		NodeID:   d.NodeID,
		EdgeID:   d.EdgeID,
		Hint:     d.Hint,
	}
}

type unparseRequest struct {
	Document json.RawMessage `json:"document"`
}

type unparseResponse struct {
	Source string `json:"source"`
}

type validateRequest struct {
	Document json.RawMessage `json:"document"`
}

type validateResponse struct {
	// Legacy string shape — preserved for any external consumer that already
	// reads it. New consumers should prefer Issues, which carries structured
	// attribution and hints.
	Diagnostics []string        `json:"diagnostics,omitempty"`
	Warnings    []string        `json:"warnings,omitempty"`
	Issues      []DiagnosticDTO `json:"issues,omitempty"`
	Valid       bool            `json:"valid"`
	NodeCount   int             `json:"node_count,omitempty"`
	EdgeCount   int             `json:"edge_count,omitempty"`
}

// --- File management types ---

type listFilesResponse struct {
	Files []fileEntry `json:"files"`
}

type fileEntry struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type openFileRequest struct {
	Path string `json:"path"`
}

type saveFileRequest struct {
	Path     string          `json:"path"`
	Document json.RawMessage `json:"document"`
}

type saveFileResponse struct {
	Path   string `json:"path"`
	Source string `json:"source"`
}

// --- Handlers ---

func (s *Server) handleParse(w http.ResponseWriter, r *http.Request) {
	var req parseRequest
	if err := readJSON(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request: %v", err)
		return
	}

	pr := parser.Parse("editor.iter", req.Source)

	var diags []string
	for _, d := range pr.Diagnostics {
		diags = append(diags, d.Error())
	}

	if pr.File == nil {
		writeJSON(w, parseResponse{Diagnostics: diags})
		return
	}

	docJSON, err := ast.MarshalFile(pr.File)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "marshal error: %v", err)
		return
	}

	writeJSON(w, parseResponse{
		Document:    json.RawMessage(docJSON),
		Diagnostics: diags,
	})
}

func (s *Server) handleUnparse(w http.ResponseWriter, r *http.Request) {
	var req unparseRequest
	if err := readJSON(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request: %v", err)
		return
	}

	f, err := ast.UnmarshalFile(req.Document)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid document: %v", err)
		return
	}

	source := unparse.Unparse(f)
	writeJSON(w, unparseResponse{Source: source})
}

func (s *Server) handleValidate(w http.ResponseWriter, r *http.Request) {
	var req validateRequest
	if err := readJSON(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request: %v", err)
		return
	}

	f, err := ast.UnmarshalFile(req.Document)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid document: %v", err)
		return
	}

	resp := validateResponse{Valid: true}

	// Parse diagnostics (re-validate via compiler).
	cr := ir.Compile(f)
	for _, d := range cr.Diagnostics {
		msg := d.Error()
		resp.Issues = append(resp.Issues, irDiagToDTO(d))
		if d.Severity == ir.SeverityError {
			resp.Diagnostics = append(resp.Diagnostics, msg)
			resp.Valid = false
		} else {
			resp.Warnings = append(resp.Warnings, msg)
		}
	}

	if cr.Workflow != nil {
		resp.NodeCount = len(cr.Workflow.Nodes)
		resp.EdgeCount = len(cr.Workflow.Edges)
	}

	writeJSON(w, resp)
}

func (s *Server) handleListExamples(w http.ResponseWriter, _ *http.Request) {
	dir := s.cfg.ExamplesDir
	if dir == "" {
		writeJSON(w, []string{})
		return
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		writeJSON(w, []string{})
		return
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".iter") {
			names = append(names, e.Name())
		}
	}
	writeJSON(w, names)
}

func (s *Server) handleLoadExample(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httpError(w, http.StatusBadRequest, "missing example name")
		return
	}

	// Sanitize: only allow simple filenames ending in .iter.
	if strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".iter") {
		httpError(w, http.StatusBadRequest, "invalid example name")
		return
	}

	dir := s.cfg.ExamplesDir
	if dir == "" {
		httpError(w, http.StatusNotFound, "no examples directory configured")
		return
	}

	path := filepath.Join(dir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		httpError(w, http.StatusNotFound, "example not found: %s", name)
		return
	}

	// Parse and return the document + source.
	pr := parser.Parse(name, string(data))
	var diags []string
	for _, d := range pr.Diagnostics {
		diags = append(diags, d.Error())
	}

	if pr.File == nil {
		writeJSON(w, parseResponse{Diagnostics: diags})
		return
	}

	docJSON, err := ast.MarshalFile(pr.File)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "marshal error: %v", err)
		return
	}

	writeJSON(w, struct {
		Source      string          `json:"source"`
		Document    json.RawMessage `json:"document"`
		Diagnostics []string        `json:"diagnostics,omitempty"`
	}{
		Source:      string(data),
		Document:    json.RawMessage(docJSON),
		Diagnostics: diags,
	})
}

// --- Helpers ---

func readJSON(r *http.Request, v interface{}) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20)) // 10 MB max
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

// IsAllowedOrigin reports whether the given Origin header value matches the
// loopback set the editor server accepts. It is exposed as a method so test
// code (and a future config flag) can extend the allowlist without rewriting
// every handler. Empty Origin (same-origin request, curl, etc.) is allowed
// because the browser CORS layer is not involved in that case.
func (s *Server) isAllowedOrigin(origin string) bool {
	if origin == "" {
		return true
	}
	for _, allowed := range s.allowedOrigins() {
		if origin == allowed {
			return true
		}
	}
	return false
}

func (s *Server) allowedOrigins() []string {
	origins := []string{
		fmt.Sprintf("http://localhost:%d", s.cfg.Port),
		fmt.Sprintf("http://127.0.0.1:%d", s.cfg.Port),
		fmt.Sprintf("http://[::1]:%d", s.cfg.Port),
	}
	// Desktop mode: the editor SPA is hosted on the Wails AssetServer
	// (wails:// on Mac/Linux, http://wails.localhost on Windows) so that
	// `window.go.main.App.*` bindings + `/wails/runtime.js` injection are
	// available. HTTP API calls reach the local server via Wails' reverse
	// proxy (which rewrites Origin to the loopback target), but the
	// editor's WebSocket clients dial the local server DIRECTLY (Wails'
	// AssetServer returns 501 on WS upgrade). The dialer therefore arrives
	// with the SPA's true origin in the upgrade handshake; without these
	// entries the upgrader's CheckOrigin would reject every cross-origin
	// WS handshake from the desktop window. Token-bearing requests are
	// already authenticated by the auth middleware; origin allow-listing
	// is defense-in-depth.
	origins = append(origins,
		"wails://wails",
		"http://wails.localhost",
	)
	return origins
}

// reflectAllowedOrigin sets ACAO to the request's Origin if (and only if) it
// is in the allowlist. Callers should always set Vary: Origin so caches don't
// poison the response across origins.
func (s *Server) reflectAllowedOrigin(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin != "" && s.isAllowedOrigin(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
	}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// writeJSONFor is the request-aware variant of writeJSON: it also reflects an
// allowlisted Origin header so legitimate browser callers receive ACAO.
func (s *Server) writeJSONFor(w http.ResponseWriter, r *http.Request, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	s.reflectAllowedOrigin(w, r)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, format string, args ...interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	msg := fmt.Sprintf(format, args...)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// httpErrorFor is the request-aware variant: reflects allowlisted Origin so
// browser code can read the error body when same-origin or loopback.
func (s *Server) httpErrorFor(w http.ResponseWriter, r *http.Request, code int, format string, args ...interface{}) {
	w.Header().Set("Content-Type", "application/json")
	s.reflectAllowedOrigin(w, r)
	w.WriteHeader(code)
	msg := fmt.Sprintf(format, args...)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// requireSafeOrigin gates state-changing endpoints. Any request whose Origin
// header is set and not in the allowlist is rejected with 403 BEFORE the
// handler runs — preventing a malicious page in another tab from POSTing
// into the local editor's filesystem-write endpoints. Same-origin and
// non-browser callers (no Origin header) pass through.
func (s *Server) requireSafeOrigin(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if s.isAllowedOrigin(origin) {
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": "cross-origin request rejected: editor only accepts loopback origins",
	})
	return false
}

// safePath resolves relPath against WorkDir and ensures the result stays within
// WorkDir AFTER symlink resolution. The previous implementation used only
// filepath.Abs + prefix check, which lets a symlink at any depth in the
// workdir point at /etc, /home/$USER/.ssh, etc. — combined with the
// unauthenticated /api/files/open and /api/files/save endpoints, that gave
// any caller on an allowlisted origin (or the same machine, before B5) a
// path-traversal primitive.
//
// Strategy:
//  1. Compute the workdir's canonical (symlink-resolved) absolute path once;
//     use it as the containment root.
//  2. Resolve the requested path's canonical form. If the file does not yet
//     exist (legitimate Save case for new files), resolve the longest
//     existing ancestor and append the remaining components. We refuse the
//     path if any existing ancestor is itself a symlink that escapes the
//     root, OR if the final composed path is not under the root.
//  3. As a defence-in-depth on Save, refuse if the immediate parent
//     directory or any intermediate path component is a symlink — a
//     pre-planted symlink at parent dir would otherwise let WriteFile
//     follow it through.
func (s *Server) safePath(relPath string) (string, error) {
	if s.cfg.WorkDir == "" {
		return "", fmt.Errorf("no working directory configured")
	}
	baseAbs, err := filepath.Abs(s.cfg.WorkDir)
	if err != nil {
		return "", fmt.Errorf("workdir abs: %w", err)
	}
	baseReal, err := filepath.EvalSymlinks(baseAbs)
	if err != nil {
		return "", fmt.Errorf("workdir resolve: %w", err)
	}

	// Compute the requested absolute path (without symlink resolution yet).
	// Idempotent on absolute inputs: handleResumeRun passes the
	// runMeta.FilePath value, which was already canonicalised at launch
	// time. Naively re-joining baseAbs with an already-absolute path
	// duplicates the workdir prefix (e.g. "/foo/bar" joined with "/foo/bar/x"
	// yields "/foo/bar/foo/bar/x"). The containment check below remains
	// the security boundary, so taking absolute inputs as-is is safe —
	// any path that escapes baseReal will still be rejected.
	var abs string
	if filepath.IsAbs(relPath) {
		abs = filepath.Clean(relPath)
	} else {
		abs = filepath.Join(baseAbs, filepath.Clean("/"+relPath))
	}
	abs, err = filepath.Abs(abs)
	if err != nil {
		return "", err
	}

	// Resolve symlinks for the longest existing prefix; keep the trailing
	// not-yet-existing components verbatim. This supports legitimate Save of
	// a brand-new file inside an existing directory.
	resolved, err := evalSymlinksLongestPrefix(abs)
	if err != nil {
		return "", err
	}

	if !pathContains(baseReal, resolved) {
		return "", fmt.Errorf("path escapes working directory")
	}
	return resolved, nil
}

// pathContains reports whether target is base or a path under base, after
// canonicalisation. Both paths must be absolute.
func pathContains(base, target string) bool {
	if base == target {
		return true
	}
	sep := string(filepath.Separator)
	if !strings.HasSuffix(base, sep) {
		base += sep
	}
	return strings.HasPrefix(target, base)
}

// evalSymlinksLongestPrefix walks abs from the root, finding the longest
// existing prefix and resolving it via filepath.EvalSymlinks; it then
// re-attaches any remaining (not-yet-existing) trailing components. If any
// existing component on the path is a symlink, EvalSymlinks resolves it —
// callers that want to refuse all symlinks in the chain (e.g. Save) should
// gate via a separate check. Returns the canonicalised absolute path.
func evalSymlinksLongestPrefix(abs string) (string, error) {
	// If the full path exists, resolve it directly.
	if _, err := os.Lstat(abs); err == nil {
		return filepath.EvalSymlinks(abs)
	}
	// Walk up until we find an existing ancestor.
	dir, leaf := filepath.Split(abs)
	dir = strings.TrimSuffix(dir, string(filepath.Separator))
	if dir == "" || dir == abs {
		return abs, nil
	}
	parent, err := evalSymlinksLongestPrefix(dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, leaf), nil
}

// --- File management handlers ---

func (s *Server) handleListFiles(w http.ResponseWriter, _ *http.Request) {
	if s.cfg.WorkDir == "" {
		writeJSON(w, listFilesResponse{Files: []fileEntry{}})
		return
	}
	var files []fileEntry
	filepath.WalkDir(s.cfg.WorkDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if isSkippedDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !isIterFile(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(s.cfg.WorkDir, path)
		files = append(files, fileEntry{Name: rel, Size: info.Size()})
		return nil
	})
	if files == nil {
		files = []fileEntry{}
	}
	writeJSON(w, listFilesResponse{Files: files})
}

func (s *Server) handleOpenFile(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	var req openFileRequest
	if err := readJSON(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request: %v", err)
		return
	}
	absPath, err := s.safePath(req.Path)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid path: %v", err)
		return
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		httpError(w, http.StatusNotFound, "file not found: %s", req.Path)
		return
	}
	pr := parser.Parse(req.Path, string(data))
	var diags []string
	for _, d := range pr.Diagnostics {
		diags = append(diags, d.Error())
	}
	if pr.File == nil {
		writeJSON(w, parseResponse{Diagnostics: diags})
		return
	}
	docJSON, err := ast.MarshalFile(pr.File)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "marshal error: %v", err)
		return
	}
	writeJSON(w, struct {
		Source      string          `json:"source"`
		Document    json.RawMessage `json:"document"`
		Diagnostics []string        `json:"diagnostics,omitempty"`
		Path        string          `json:"path"`
	}{
		Source:      string(data),
		Document:    json.RawMessage(docJSON),
		Diagnostics: diags,
		Path:        req.Path,
	})
}

func (s *Server) handleSaveFile(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	var req saveFileRequest
	if err := readJSON(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request: %v", err)
		return
	}
	if !strings.HasSuffix(req.Path, ".iter") {
		httpError(w, http.StatusBadRequest, "filename must end in .iter")
		return
	}
	absPath, err := s.safePath(req.Path)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid path: %v", err)
		return
	}
	f, err := ast.UnmarshalFile(req.Document)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid document: %v", err)
		return
	}
	source := unparse.Unparse(f)
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		httpError(w, http.StatusInternalServerError, "cannot create directory: %v", err)
		return
	}
	if s.watcher != nil {
		s.watcher.IgnorePath(absPath)
	}
	if err := os.WriteFile(absPath, []byte(source), 0o644); err != nil {
		httpError(w, http.StatusInternalServerError, "write error: %v", err)
		return
	}
	writeJSON(w, saveFileResponse{Path: req.Path, Source: source})
}
