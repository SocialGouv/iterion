package cli

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/SocialGouv/iterion/server"
)

// EditorOptions holds options for the editor command.
type EditorOptions struct {
	Port        int
	Dir         string // working directory (for examples)
	NoBrowser   bool   // skip opening browser
}

// RunEditor starts the editor HTTP server.
func RunEditor(ctx context.Context, opts EditorOptions, p *Printer) error {
	if opts.Port == 0 {
		opts.Port = 4891
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
		Port:        opts.Port,
		ExamplesDir: examplesDir,
		WorkDir:     dir,
		OpenBrowser: !opts.NoBrowser,
	}

	srv := server.New(cfg)

	// Open browser in background.
	if !opts.NoBrowser {
		go openBrowser(fmt.Sprintf("http://localhost:%d", opts.Port))
	}

	if p.Format == OutputHuman {
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

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*1e9) // 5s
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
	if err := cmd.Start(); err != nil {
		log.Printf("failed to open browser: %v", err)
	}
}
