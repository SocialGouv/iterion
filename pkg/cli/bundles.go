package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// requireWorkflowPathExists fails early with a clear, actionable error
// when path (a `.bot` file, `.botz` archive, or bundle directory) does
// not exist. It MUST be called on the already-resolved path (i.e. after
// ResolveRecipePath), so embedded catalog-bot names — which resolve to
// an existing cache file — are never rejected.
//
// Only the not-exist case is handled here; any other stat error
// (permission, etc.) returns nil so the downstream openers surface their
// own context. Without this, run / validate / diagram all bottom out in
// a low-level os.Stat / os.ReadFile error that leaks Go internals, shows
// the relative path the user typed, and offers no guidance. The returned
// error is wrapped with ErrUserInput (exit code 2) and embeds the hint in
// its message because the plain fmt.Errorf CLI path has no Hint channel.
func requireWorkflowPathExists(path string) error {
	_, err := os.Stat(path)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		// Exists, or a non-not-found stat error the downstream openers
		// will surface with their own context.
		return nil
	}
	abs, absErr := filepath.Abs(path)
	if absErr != nil {
		abs = path
	}
	return UserInputError(fmt.Errorf(
		"workflow file not found: %s\n"+
			"  - check the path is correct (you passed: %s)\n"+
			"  - run from the repo root, or pass an absolute path\n"+
			"  - run `iterion bots list` to see available bots",
		abs, path))
}

// openBundleOrFile applies the standard bundle dispatch — detect →
// open archive or directory — that the CLI's run / resume / sandbox-
// doctor paths all share. When path is neither a `.botz` archive nor
// a bundle directory it returns (nil, path, no-op cleanup, nil) so
// callers can fall through to a plain `.bot` compile without
// branching on Detect's Kind themselves.
//
// Errors are returned bare (Detect / Open / OpenDir all surface
// their own context); the caller wraps with its own subcommand
// prefix so resume / run / doctor each keep their bespoke messages.
//
// The returned cleanup MUST be deferred by the caller. It is a no-op
// for bundle directories and for the non-bundle pass-through; only
// the `.botz` archive path carries a real temp-dir cleanup today,
// but callers should still defer unconditionally so future per-run
// extraction modes stay safe. On error the cleanup is the no-op (the
// underlying bundle openers release their own resources before
// returning an error), so callers can `defer cleanup()` before
// checking err without leaking.
//
// The returned Kind lets callers differentiate the archive vs.
// directory branches when their error wrapping must mention the
// distinction (e.g. resume's "original archive may have moved" hint).
func openBundleOrFile(path string) (b *bundle.Bundle, iterPath string, kind bundle.Kind, cleanup func() error, err error) {
	cleanup = func() error { return nil }
	kind, detectErr := bundle.Detect(path)
	if detectErr != nil {
		return nil, path, kind, cleanup, detectErr
	}
	switch kind {
	case bundle.KindBundle:
		opened, c, openErr := bundle.Open(path, "")
		if openErr != nil {
			return nil, path, kind, cleanup, openErr
		}
		return opened, opened.IterPath, kind, c, nil
	case bundle.KindBundleDir:
		opened, openErr := bundle.OpenDir(path)
		if openErr != nil {
			return nil, path, kind, cleanup, openErr
		}
		return opened, opened.IterPath, kind, cleanup, nil
	}
	// A bare main.bot whose parent is a bundle (a manifest or a skills/
	// beside it) is promoted to that bundle, so every surface that opens a
	// workflow — run, resume, validate, doctor — sees the prompts/*.md and
	// the skills/ it ships; a bot whose prompts live in prompts/ would
	// otherwise validate INVALID (C003) as a file and OK as a directory.
	// Conservative on purpose: what counts as a bundle is pkg/bundle's to
	// decide (DirForMainBot). A parent that only looks like one and does not
	// open falls through to the bare file, as run always did.
	if parent := bundle.DirForMainBot(path); parent != "" {
		if opened, openErr := bundle.OpenDir(parent); openErr == nil {
			return opened, opened.IterPath, bundle.KindBundleDir, cleanup, nil
		}
	}
	return nil, path, kind, cleanup, nil
}
