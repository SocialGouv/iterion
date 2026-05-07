package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/server"
)

// EditorOptions holds options for the editor command.
type EditorOptions struct {
	Port      int
	Bind      string // bind address (default "127.0.0.1"); use "0.0.0.0" to expose on LAN
	Dir       string // working directory (for examples)
	StoreDir  string // run store directory (default: nearest .iterion ancestor of Dir, or <Dir>/.iterion)
	NoBrowser bool   // skip opening browser
	// NoBrowserPane disables every Browser-pane code path: the
	// preview proxy, the CDP WS endpoint, and the Chromium runner.
	// Useful for emergency lockdown (security incident) or for
	// shaving startup latency when the operator never needs the
	// pane. Defaults to false (pane enabled).
	NoBrowserPane bool

	// OnReady, when non-nil, is invoked once the HTTP listener is up and
	// the server has accepted its bind address. The argument is the actual
	// host:port the listener is bound to (useful when Port=0 / random).
	// Invoked from a goroutine; callers must be ready for it to fire
	// concurrently with their own Run loop.
	OnReady func(addr string)

	// Mode, when set, advertises the deployment context in
	// /api/server/info and tunes upload defaults ("desktop" raises
	// the per-file cap to 1 GB; "" / "local" / "web" keep the 50 MB
	// cap). The server's Config.Mode owns the same field.
	Mode string

	// MaxUploadSize / MaxTotalUploadSize / MaxUploadsPerRun /
	// AllowUploadMime override the server's upload limits. Zero
	// values fall through to mode-specific defaults applied by
	// pkg/server.applyUploadDefaults.
	MaxUploadSize      int64
	MaxTotalUploadSize int64
	MaxUploadsPerRun   int
	AllowUploadMime    []string
}

// RunEditor starts the editor HTTP server.
//
// Port semantics:
//   - opts.Port == 0  : default to 4891 (legacy CLI behaviour — the
//     `iterion editor` cobra command relies on this when --port is omitted).
//   - opts.Port == -1 : let the OS pick a free port. The actual bind address
//     is delivered via opts.OnReady. Used by the desktop host so multiple
//     instances/projects never fight for 4891.
//   - opts.Port > 0   : use that exact port.
func RunEditor(ctx context.Context, opts EditorOptions, p *Printer) error {
	switch {
	case opts.Port == 0:
		opts.Port = 4891
	case opts.Port == -1:
		opts.Port = 0
	}
	if opts.Bind == "" {
		opts.Bind = "127.0.0.1"
	}

	dir := opts.Dir
	if dir == "" {
		dir, _ = os.Getwd()
	}

	// Look for examples directory.
	examplesDir := filepath.Join(dir, "examples")
	if _, err := os.Stat(examplesDir); err != nil {
		examplesDir = ""
	}

	cfg := server.Config{
		Port:               opts.Port,
		Bind:               opts.Bind,
		ExamplesDir:        examplesDir,
		WorkDir:            dir,
		StoreDir:           opts.StoreDir,
		OpenBrowser:        !opts.NoBrowser,
		Mode:               opts.Mode,
		MaxUploadSize:      opts.MaxUploadSize,
		MaxTotalUploadSize: opts.MaxTotalUploadSize,
		MaxUploadsPerRun:   opts.MaxUploadsPerRun,
		AllowedUploadMIMEs: opts.AllowUploadMime,
		// Local mode: the editor process is implicitly trusted to
		// its TTY user. CSRF protection still gates write endpoints
		// via Origin allowlisting; cross-tenant isolation does not
		// apply because there is exactly one local user.
		DisableAuth: true,
	}
	// Wire the in-memory BrowserRegistry unless the operator
	// explicitly disabled the pane. The registry is process-local;
	// the runtime + iterion __browser-attach command share it via
	// the Server reference exposed below.
	if !opts.NoBrowserPane {
		cfg.BrowserRegistry = mcp.NewMemoryBrowserRegistry()
	}

	logger := iterlog.New(iterlog.LevelInfo, os.Stderr)
	srv := server.New(cfg, logger)

	// Open browser in background — only meaningful when port is fixed
	// upfront. For Port=0 (random) callers should use OnReady.
	if !opts.NoBrowser && opts.Port != 0 {
		go openBrowser(fmt.Sprintf("http://localhost:%d", opts.Port))
	}

	if p.Format == OutputHuman && opts.Port != 0 {
		p.Header("Iterion Editor")
		p.KV("URL", fmt.Sprintf("http://localhost:%d", opts.Port))
		if examplesDir != "" {
			p.KV("Examples", examplesDir)
		}
		p.Blank()
		p.Line("  Press Ctrl+C to stop.")
	}

	// Run server until context is cancelled.
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	// Notify the caller once the listener is bound. Addr() blocks until
	// then. We run this in its own goroutine to avoid stalling shutdown
	// if the listener never comes up — the addrReady channel is closed
	// on bind failure too, so OnReady fires with the empty string and
	// the caller can check.
	if opts.OnReady != nil {
		go func() {
			addr := srv.Addr()
			if addr != "" && opts.OnReady != nil {
				opts.OnReady(addr)
			}
		}()
	}

	select {
	case <-ctx.Done():
		// 60 s drain budget: long enough to let in-flight runs reach
		// their cancel points + flip persisted status to
		// failed_resumable, but bounded so a wedged subprocess can't
		// hold the editor process forever. Server.Shutdown calls
		// runs.Drain (which uses this ctx) and then http.Server.Shutdown
		// in sequence, so the budget is shared across both phases.
		if p.Format == OutputHuman {
			p.Line("\nShutdown signal received — draining in-flight runs (up to 60s)…")
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
