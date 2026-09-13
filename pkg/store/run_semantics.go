package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	RuntimeSemanticsPortsV1 = "ports-v1"
	// An adapter keeps the legacy control interpreter inside the native run
	// namespace. Its descendants must retain that namespace too.
	RuntimeSemanticsLegacyAdapterV1 = "legacy-adapter-v1"
	NativeRunIDPrefix               = "pc1_"
	NativeRunsDirectory             = "port_runs_v1"
	NativeRunFormatVersion          = 2
)

var ErrRunSemantics = errors.New("unsupported or mismatched run semantics")

var reservedRunPrefix = regexp.MustCompile(`^pc[0-9]+_`)

// IsNativeRunID classifies routing, not validity. ValidateRunID must precede
// any access. In particular a malformed reserved ID must never fall back to
// legacy paths, collections or blob keys.
func IsNativeRunID(id string) bool { return strings.HasPrefix(id, NativeRunIDPrefix) }

func ValidateRunID(id string) error {
	if err := SanitizePathComponent("run ID", id); err != nil {
		return err
	}
	if prefix := reservedRunPrefix.FindString(id); prefix != "" {
		if prefix != NativeRunIDPrefix || len(id) == len(prefix) {
			return fmt.Errorf("store: run ID %q: %w", id, ErrRunSemantics)
		}
	}
	return nil
}

// RunDataDirectory is the directory family relative to an existing store
// root. Unsupported reserved versions have no route to legacy data.
func RunDataDirectory(id string) string {
	if IsNativeRunID(id) {
		return NativeRunsDirectory
	}
	if reservedRunPrefix.MatchString(id) {
		return ".unsupported_port_runs"
	}
	return "runs"
}

// RunBlobPrefix puts the entire native blob closure outside every key family
// built by supported older clients, including their blind delete helpers.
func RunBlobPrefix(id string) string {
	if IsNativeRunID(id) {
		return "ports-v1/"
	}
	if reservedRunPrefix.MatchString(id) {
		return "unsupported-ports/"
	}
	return ""
}

type runtimeSemanticsKey struct{}

// WithRuntimeSemantics carries the explicit creation contract through the
// existing RunStore creation seam. A reserved ID alone is never authorization
// to create a native run. Launch coordinators must check activation before
// selecting this context; this storage helper does not grant activation.
func WithRuntimeSemantics(ctx context.Context, semantics string) context.Context {
	return context.WithValue(ctx, runtimeSemanticsKey{}, semantics)
}

func StampRunSemantics(ctx context.Context, r *Run) error {
	semantics, _ := ctx.Value(runtimeSemanticsKey{}).(string)
	r.RuntimeSemantics = semantics
	if IsNativeRunID(r.ID) {
		r.FormatVersion = NativeRunFormatVersion
	}
	return ValidateRunSemantics(r)
}

func ValidateRunSemantics(r *Run) error {
	if r == nil {
		return fmt.Errorf("store: nil run: %w", ErrRunSemantics)
	}
	if err := ValidateRunID(r.ID); err != nil {
		return err
	}
	if r.ParentRunID != "" {
		if err := ValidateRunID(r.ParentRunID); err != nil {
			return err
		}
		if IsNativeRunID(r.ParentRunID) && !IsNativeRunID(r.ID) {
			return fmt.Errorf("store: child %s escapes native parent %s: %w", r.ID, r.ParentRunID, ErrRunSemantics)
		}
	}
	if IsNativeRunID(r.ID) {
		for _, child := range r.SubbotChildren {
			if err := ValidateNativeChild(r.ID, child); err != nil {
				return err
			}
		}
		if r.FormatVersion != NativeRunFormatVersion ||
			(r.RuntimeSemantics != RuntimeSemanticsPortsV1 && r.RuntimeSemantics != RuntimeSemanticsLegacyAdapterV1) {
			return fmt.Errorf("store: native run %s requires explicit supported semantics and format: %w", r.ID, ErrRunSemantics)
		}
	} else if r.RuntimeSemantics != "" {
		return fmt.Errorf("store: run %s requires a native ID for %s: %w", r.ID, r.RuntimeSemantics, ErrRunSemantics)
	}
	return nil
}

func ValidateNativeChild(parentID, childID string) error {
	if !IsNativeRunID(parentID) {
		return nil
	}
	if err := ValidateRunID(childID); err != nil {
		return err
	}
	if !IsNativeRunID(childID) {
		return fmt.Errorf("store: child %s escapes native parent %s: %w", childID, parentID, ErrRunSemantics)
	}
	return nil
}

// CheckRunSemanticIdentity prevents forced resume and stale full-document
// writers from changing the interpreter behind an existing run identity.
func CheckRunSemanticIdentity(current, next *Run) error {
	if err := ValidateRunSemantics(next); err != nil {
		return err
	}
	if current.ID != next.ID || current.RuntimeSemantics != next.RuntimeSemantics ||
		(IsNativeRunID(current.ID) && current.FormatVersion != next.FormatVersion) {
		return fmt.Errorf("store: cannot change semantics of run %s: %w", current.ID, ErrRunSemantics)
	}
	return nil
}

func (s *FilesystemRunStore) guardNativeRun(id string) error {
	if err := ValidateRunID(id); err != nil {
		return err
	}
	if IsNativeRunID(id) {
		_, err := s.loadRunRaw(id)
		return err
	}
	return nil
}
