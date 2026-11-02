package secrets

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// DefaultMaxContextBytes bounds what one bound context document may weigh
// when the caller has no opinion. It matches the attachment store's upload
// cap order (a context IS delivered as a run attachment), and it is the
// only bound the local filesystem store's WriteAttachment — which trusts
// its caller — ever gets.
const DefaultMaxContextBytes = 50 << 20

// ContextFetchFunc materialises one context binding's pinned source into a
// local checkout and returns the checkout's root directory. It is the seam
// the server wires onto pluginsource.Fetcher: the pin doctrine, the cache
// and the credential-by-reference handling are that fetcher's, this package
// only names what a binding needs from one.
type ContextFetchFunc func(ctx context.Context, b BotSecretBinding) (checkoutDir string, err error)

// ResolvedContext is one context binding materialised for a launch: the
// bytes of Context.Path, read out of the pinned checkout, plus what a run
// attachment needs to carry them.
type ResolvedContext struct {
	// Name is the workflow-declared `attachments:` name the binding
	// resolves under (ADR-147 D6).
	Name string
	// Filename is the base name of the file inside the source, kept so the
	// attachment's resolved path stays human-readable to the agent.
	Filename string
	MIME     string
	Content  []byte
}

// SkippedContext names a context binding a launch proceeded WITHOUT, and
// why. The durable readout is the binding's own degraded flag; this slice
// is what lets the launch say so against the run it is launching.
type SkippedContext struct {
	Binding BotSecretBinding
	Err     error
}

// ResolveContextBindings resolves every `context` binding of one bot into
// the bytes a launch must deliver, under the names the workflow declares.
//
// Failure policy (ADR-147 D3, the pluginsource.Resolver discipline): a
// binding that cannot be materialised — an undeclared name, a fetch the
// remote refuses, a path the checkout does not carry, a document past the
// size bound — is SKIPPED for this launch and flagged degraded on its
// record, never fatal: one team's broken binding must not fail every launch
// of the bot, and the skip is not quiet (the record shows it, the caller
// logs it against the run). The only fatal error left is the LIST failing:
// then nothing can tell a healthy bot from one whose contexts are all lost.
//
// declared is the workflow's `attachments:` name set: a binding under a
// name the workflow never declares would deliver a file no node can reach
// (D6), so it is skipped and said so. maxBytes bounds one document; zero
// reads DefaultMaxContextBytes.
func ResolveContextBindings(
	ctx context.Context,
	bindings BotSecretBindingStore,
	fetch ContextFetchFunc,
	tenantID, botID string,
	declared map[string]bool,
	maxBytes int64,
	logger *iterlog.Logger,
) ([]ResolvedContext, []SkippedContext, error) {
	if bindings == nil || fetch == nil || tenantID == "" || botID == "" {
		return nil, nil, nil
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxContextBytes
	}
	list, err := bindings.ListByTenantBot(ctx, tenantID, botID)
	if err != nil {
		return nil, nil, fmt.Errorf("secrets: list bindings for bot %s: %w", botID, err)
	}
	var resolved []ResolvedContext
	var skipped []SkippedContext
	for _, b := range list {
		if b.EffectiveKind() != BindingKindContext {
			continue
		}
		rc, skipErr := materialiseContextBinding(ctx, fetch, b, declared, maxBytes)
		if skipErr != nil {
			skipped = append(skipped, SkippedContext{Binding: b, Err: skipErr})
			// The readout is best-effort next to the skip itself: a store
			// that cannot take the stamp still gets the launch, and the
			// caller's log line names the binding either way.
			if merr := bindings.MarkDegraded(ctx, tenantID, b.ID, skipErr.Error()); merr != nil && logger != nil {
				logger.Warn("secrets: context binding %q (bot=%s) could not be flagged degraded after a failed materialisation: %v", b.DeclaredName(), botID, merr)
			}
			continue
		}
		if b.Degraded() {
			if cerr := bindings.ClearDegraded(ctx, tenantID, b.ID); cerr != nil && logger != nil {
				logger.Warn("secrets: context binding %q (bot=%s) materialised again but its degraded flag could not be cleared: %v", b.DeclaredName(), botID, cerr)
			}
		}
		resolved = append(resolved, rc)
	}
	return resolved, skipped, nil
}

// materialiseContextBinding resolves ONE binding: the declaration gate,
// the fetch, and the bounded safe read of the pinned file.
func materialiseContextBinding(ctx context.Context, fetch ContextFetchFunc, b BotSecretBinding, declared map[string]bool, maxBytes int64) (ResolvedContext, error) {
	name := b.DeclaredName()
	if !declared[name] {
		return ResolvedContext{}, fmt.Errorf("the workflow declares no attachments: name %q — declare it or the delivered file is one no node can reach", name)
	}
	dir, err := fetch(ctx, b)
	if err != nil {
		return ResolvedContext{}, fmt.Errorf("fetch the pinned source: %w", err)
	}
	content, err := readContextFile(dir, b.Context.Path, maxBytes)
	if err != nil {
		return ResolvedContext{}, err
	}
	mime := http.DetectContentType(content)
	return ResolvedContext{
		Name:     name,
		Filename: path.Base(path.Clean(b.Context.Path)),
		MIME:     mime,
		Content:  content,
	}, nil
}

// readContextFile reads one file out of a fetched checkout, at the binding's
// declared path. The checkout's CONTENT is untrusted input — an operator's
// pinned repo can commit a symlink at the bound path, and a plain os.Open
// would follow it outside the checkout and deliver an arbitrary host file
// into every run of the bot. os.OpenRoot refuses that escape natively
// (component-wise, no-following), and the lexical check ahead of it turns an
// absolute or escaping path into the operator's own misconfiguration rather
// than the runtime's. The read is bounded: past maxBytes the document is
// refused whole, not truncated — a truncated reference document reads as
// complete to the agent.
func readContextFile(checkoutDir, relPath string, maxBytes int64) ([]byte, error) {
	cleaned, err := cleanContextRelPath(relPath)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(checkoutDir)
	if err != nil {
		return nil, fmt.Errorf("open the fetched checkout: %w", err)
	}
	defer root.Close()
	f, err := root.Open(cleaned)
	if err != nil {
		return nil, fmt.Errorf("open %q in the fetched source: %w", relPath, err)
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.IsDir() {
		return nil, fmt.Errorf("%q is a directory in the fetched source, not a file", relPath)
	}
	buf, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", relPath, err)
	}
	if int64(len(buf)) > maxBytes {
		return nil, fmt.Errorf("%q exceeds the %d-byte context ceiling — bind a smaller document or raise the bound where the attachment quota is set", relPath, maxBytes)
	}
	return buf, nil
}

// cleanContextRelPath validates the binding's in-source path: relative,
// slash-clean, and never escaping its root. It is the lexical half of the
// traversal defence; the open half is readContextFile's rooted open.
func cleanContextRelPath(relPath string) (string, error) {
	p := strings.TrimSpace(relPath)
	if p == "" {
		return "", fmt.Errorf("the binding's context.path is empty")
	}
	p = strings.ReplaceAll(p, "\\", "/")
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("context.path %q must be relative to the source root", relPath)
	}
	cleaned := path.Clean(p)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("context.path %q escapes the source root", relPath)
	}
	return cleaned, nil
}
