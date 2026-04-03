package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/errors"
)

const (
	// BinaryName is the name of the Codex CLI binary.
	BinaryName = "codex"

	// MinimumVersion is the minimum required Codex CLI version.
	MinimumVersion = "0.103.0"

	// VersionCheckTimeout is the timeout for the CLI version check command.
	VersionCheckTimeout = 2 * time.Second
)

var versionRegex = regexp.MustCompile(`(\d+\.\d+\.\d+)`)

// Config holds configuration for CLI discovery.
type Config struct {
	// CliPath is an explicit CLI path that skips PATH search.
	CliPath string

	// SkipVersionCheck skips version validation during discovery.
	// Can also be controlled via CODEX_CLI_SKIP_VERSION_CHECK env var.
	SkipVersionCheck bool

	// Logger is an optional logger for discovery operations.
	Logger *slog.Logger
}

// Discoverer locates and validates the Codex CLI binary.
type Discoverer interface {
	// Discover locates the Codex CLI binary and validates its version.
	Discover(ctx context.Context) (string, error)
}

// discoverer implements the Discoverer interface.
type discoverer struct {
	cfg *Config
	log *slog.Logger
}

// Compile-time verification that discoverer implements Discoverer.
var _ Discoverer = (*discoverer)(nil)

// NewDiscoverer creates a new CLI discoverer with the given configuration.
func NewDiscoverer(cfg *Config) Discoverer {
	if cfg == nil {
		cfg = &Config{}
	}

	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}))
	}

	return &discoverer{
		cfg: cfg,
		log: log,
	}
}

// Discover locates the Codex CLI binary and validates its version.
func (d *discoverer) Discover(ctx context.Context) (string, error) {
	d.log.DebugContext(ctx, "discovering Codex CLI binary")

	cliPath, err := d.findCLI(ctx)
	if err != nil {
		d.log.ErrorContext(ctx, "failed to find Codex CLI", slog.String("error", err.Error()))

		return "", err
	}

	d.log.DebugContext(ctx, "found Codex CLI binary", slog.String("cli_path", cliPath))

	d.checkVersion(ctx, cliPath)

	return cliPath, nil
}

// findCLI locates the Codex CLI binary.
func (d *discoverer) findCLI(ctx context.Context) (string, error) {
	// If explicit path provided, use it
	if d.cfg.CliPath != "" {
		d.log.DebugContext(ctx, "using explicit CLI path", slog.String("cli_path", d.cfg.CliPath))

		if isExecutable(d.cfg.CliPath) {
			return d.cfg.CliPath, nil
		}

		return "", &errors.CLINotFoundError{SearchedPaths: []string{d.cfg.CliPath}}
	}

	searchedPaths := make([]string, 0, 6)

	// Search in PATH
	if path, err := exec.LookPath(BinaryName); err == nil {
		d.log.DebugContext(ctx, "found in PATH", slog.String("path", path))

		return path, nil
	}

	searchedPaths = append(searchedPaths, "$PATH")

	// Check common locations
	home, _ := os.UserHomeDir()
	candidates := commonPaths(home)

	for _, path := range candidates {
		searchedPaths = append(searchedPaths, path)

		if isExecutable(path) {
			d.log.DebugContext(ctx, "found at common location", slog.String("path", path))

			return path, nil
		}
	}

	d.log.WarnContext(ctx, "Codex CLI not found", slog.Any("searched_paths", searchedPaths))

	return "", &errors.CLINotFoundError{SearchedPaths: searchedPaths}
}

// commonPaths returns well-known installation paths for the Codex CLI.
func commonPaths(home string) []string {
	paths := make([]string, 0, 4)

	if home != "" {
		paths = append(paths,
			filepath.Join(home, ".volta", "bin", BinaryName),
			filepath.Join(home, ".local", "bin", BinaryName),
		)
	}

	paths = append(paths,
		filepath.Join("/usr", "local", "bin", BinaryName),
		filepath.Join("/usr", "bin", BinaryName),
	)

	return paths
}

// isExecutable checks that the path exists and is executable.
func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}

	return !info.IsDir() && info.Mode()&0o111 != 0
}

// checkVersion checks if the Codex CLI version meets minimum requirements.
// Logs a warning if version is below minimum. Errors are silently ignored.
func (d *discoverer) checkVersion(ctx context.Context, cliPath string) {
	if d.cfg.SkipVersionCheck {
		d.log.DebugContext(ctx, "skipping CLI version check (configured)")

		return
	}

	if os.Getenv("CODEX_CLI_SKIP_VERSION_CHECK") != "" {
		d.log.DebugContext(ctx, "skipping CLI version check (CODEX_CLI_SKIP_VERSION_CHECK set)")

		return
	}

	vctx, cancel := context.WithTimeout(ctx, VersionCheckTimeout)
	defer cancel()

	out, err := exec.CommandContext(vctx, cliPath, "--version").Output()
	if err != nil {
		d.log.DebugContext(ctx, "CLI version check failed", slog.String("error", err.Error()))

		return
	}

	version := parseVersion(string(out))
	if version == "" {
		d.log.DebugContext(ctx, "could not parse CLI version",
			slog.String("output", strings.TrimSpace(string(out))),
		)

		return
	}

	if compareVersions(version, MinimumVersion) < 0 {
		d.log.WarnContext(ctx, "Codex CLI version is below minimum",
			slog.String("version", version),
			slog.String("minimum_required", MinimumVersion),
		)

		fmt.Fprintf(os.Stderr,
			"Warning: Codex CLI version %s is below minimum required version %s. "+
				"Some features may not work correctly.\n",
			version, MinimumVersion,
		)
	} else {
		d.log.DebugContext(ctx, "CLI version check passed",
			slog.String("version", version),
			slog.String("minimum", MinimumVersion),
		)
	}
}

// parseVersion extracts a semantic version from CLI output.
func parseVersion(output string) string {
	return versionRegex.FindString(output)
}

// compareVersions compares two semantic versions.
// Returns -1 if a < b, 0 if a == b, 1 if a > b.
func compareVersions(a, b string) int {
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")

	for i := range 3 {
		aNum := 0
		bNum := 0

		if i < len(aParts) {
			aNum, _ = strconv.Atoi(aParts[i])
		}

		if i < len(bParts) {
			bNum, _ = strconv.Atoi(bParts[i])
		}

		if aNum < bNum {
			return -1
		}

		if aNum > bNum {
			return 1
		}
	}

	return 0
}
