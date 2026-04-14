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
	"time"

	"github.com/SocialGouv/iterion/astjson"
	"github.com/SocialGouv/iterion/ir"
	iterlog "github.com/SocialGouv/iterion/log"
	"github.com/SocialGouv/iterion/parser"
	"github.com/SocialGouv/iterion/unparse"
)

//go:embed static
var staticFS embed.FS

// Config holds the server configuration.
type Config struct {
	Port        int    // HTTP port (default 4891)
	ExamplesDir string // path to examples directory
	WorkDir     string // root directory for file operations
	OpenBrowser bool   // open browser on start
}

// Server is the editor HTTP server.
type Server struct {
	cfg     Config
	logger  *iterlog.Logger
	mux     *http.ServeMux
	server  *http.Server
	hub     *Hub
	watcher *Watcher
}

// New creates a new editor server.
func New(cfg Config, logger *iterlog.Logger) *Server {
	if cfg.Port == 0 {
		cfg.Port = 4891
	}
	s := &Server{cfg: cfg, logger: logger, mux: http.NewServeMux()}
	s.hub = NewHub(logger)
	go s.hub.Run()
	if cfg.WorkDir != "" {
		var err error
		s.watcher, err = NewWatcher(cfg.WorkDir, s.hub, logger)
		if err != nil {
			logger.Warn("file watcher disabled: %v", err)
		} else {
			go s.watcher.Start()
		}
	}
	s.routes()
	s.server = &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// ListenAndServe starts the HTTP server.
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		return err
	}
	s.logger.Info("Editor server listening on http://localhost:%d", s.cfg.Port)
	return s.server.Serve(ln)
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.watcher != nil {
		s.watcher.Stop()
	}
	s.hub.Stop()
	return s.server.Shutdown(ctx)
}

func (s *Server) routes() {
	// CORS preflight handler
	s.mux.HandleFunc("OPTIONS /api/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.WriteHeader(http.StatusNoContent)
	})

	s.mux.HandleFunc("POST /api/parse", s.handleParse)
	s.mux.HandleFunc("POST /api/unparse", s.handleUnparse)
	s.mux.HandleFunc("POST /api/validate", s.handleValidate)
	s.mux.HandleFunc("GET /api/examples", s.handleListExamples)
	s.mux.HandleFunc("GET /api/examples/{name...}", s.handleLoadExample)

	// WebSocket endpoint for file watching
	s.mux.HandleFunc("GET /api/ws", s.hub.HandleWebSocket)

	// File management endpoints
	s.mux.HandleFunc("GET /api/files", s.handleListFiles)
	s.mux.HandleFunc("POST /api/files/open", s.handleOpenFile)
	s.mux.HandleFunc("POST /api/files/save", s.handleSaveFile)

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
	Diagnostics []string `json:"diagnostics,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
	Valid       bool     `json:"valid"`
	NodeCount   int      `json:"node_count,omitempty"`
	EdgeCount   int      `json:"edge_count,omitempty"`
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

	docJSON, err := astjson.Marshal(pr.File)
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

	f, err := astjson.Unmarshal(req.Document)
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

	f, err := astjson.Unmarshal(req.Document)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid document: %v", err)
		return
	}

	resp := validateResponse{Valid: true}

	// Parse diagnostics (re-validate via compiler).
	cr := ir.Compile(f)
	for _, d := range cr.Diagnostics {
		msg := d.Error()
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

	docJSON, err := astjson.Marshal(pr.File)
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

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, format string, args ...interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(code)
	msg := fmt.Sprintf(format, args...)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// safePath resolves relPath against WorkDir and ensures the result stays within WorkDir.
func (s *Server) safePath(relPath string) (string, error) {
	if s.cfg.WorkDir == "" {
		return "", fmt.Errorf("no working directory configured")
	}
	abs := filepath.Join(s.cfg.WorkDir, filepath.Clean("/"+relPath))
	abs, err := filepath.Abs(abs)
	if err != nil {
		return "", err
	}
	base, _ := filepath.Abs(s.cfg.WorkDir)
	if !strings.HasPrefix(abs, base+string(filepath.Separator)) && abs != base {
		return "", fmt.Errorf("path escapes working directory")
	}
	return abs, nil
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
	docJSON, err := astjson.Marshal(pr.File)
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
	f, err := astjson.Unmarshal(req.Document)
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
