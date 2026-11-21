package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/runview"
)

// botSourceChangedErrorCode tells a client that a pinned launch was refused
// because the bot's program is no longer the one the pin names
// (launchRunRequest.ExpectSourceSHA256): a file of it was edited, or another
// tier now serves the name, since the launch form read it. Nothing was
// published and the admission was handed back; the remedy is to read the
// program again (GET /api/v1/bots/{name}/launch-source) and launch what it
// answers.
const botSourceChangedErrorCode = "bot_source_changed"

// launchPinUnsupportedErrorCode tells a client that a pinned launch was
// refused because this launch cannot hold a pin, whatever the pin says: a pin
// is the cloud lane's guarantee (runview.ErrLaunchPinUnsupported). Nothing was
// compared, published or metered; the remedy is to launch without it.
const launchPinUnsupportedErrorCode = "launch_pin_unsupported"

// launchPinUnsupportedSentence is that refusal, in the caller's words.
const launchPinUnsupportedSentence = "expect_source_sha256 is honoured where the launch runs a frozen program — the cloud lane. This server reads the bot's files again after it accepts a launch, so a pin here would be compared and then outrun: launch without it"

// launchPinPattern is the shape of a pin: the sha256 the launch-program read
// issued, as it wrote it — 64 lowercase hex digits.
var launchPinPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// botLaunchSourceResponse is the program a launch naming this bot runs, as
// the caller's active team resolves it now: what a launch form renders its
// fields from, and the digest it then pins its launch to.
type botLaunchSourceResponse struct {
	// BotID is the canonical id of the tier that serves — the identity a
	// launch naming this bot is stamped with (LaunchSpec.BotID).
	BotID string `json:"bot_id"`
	// Tier is the tier that serves, in the words a run records
	// (store.BotSourceTier*): "team", "platform" or "baked".
	Tier string `json:"tier"`
	// Source is the entry source the launch compiles.
	Source string `json:"source"`
	// SHA256 is the identity of the WHOLE program the launch compiles — the
	// entry source, the bundle's prompts/ and presets/, the fragments and
	// the files its {{include}} markers read (runview.CompiledSource.Hash,
	// the workflow_hash a run records). It is what a launch presents back
	// as expect_source_sha256: everything this answer shows is under it.
	SHA256 string `json:"sha256"`
	// Document, Diagnostics, Unit, Bindable and ReadRefusal are the open of
	// that source, as /api/files/open answers a file (openWorkflow). Document
	// is the AST as a JSON object; it is declared `any` because the OpenAPI
	// generator renders a json.RawMessage as a base64 string, which the
	// answer is not.
	Document    any              `json:"document"`
	Diagnostics []string         `json:"diagnostics,omitempty"`
	Unit        *unitInfo        `json:"unit,omitempty"`
	Bindable    bool             `json:"bindable"`
	ReadRefusal *readRefusalInfo `json:"read_refusal,omitempty"`
	// Presets is the preset set a launch of this program applies: the
	// program's own `presets:` block overlaid by the bundle's
	// presets/<name>.md, a file winning on a name (the registry's rule,
	// botregistry.LoadSchema — and the run's, runtime.MergeBundlePresets).
	// It is read from the materialization the digest covers, so a form
	// built on it shows the values the launch applies.
	Presets *botregistry.PresetsBlock `json:"presets,omitempty"`
}

// botLaunchSourceReadable reports whether the caller may read, through the
// launch-program route, the text of a tier — the ONE rule of that read, asked
// at the one door every bundle of a resolution comes through
// (withTierReadGuard, rawLaunchBotOfTier): for the bot named and for every
// bundle its freeze resolves, on the route and on a pinned launch alike.
//   - a team row: the right to edit the bots of the team the request is
//     scoped to (editsScopedTeamBots), the gate of
//     GET /api/teams/{id}/bot-sources/{slug};
//   - a platform row and the baked catalog: any authenticated caller — one
//     with an active team, in practice, since the authentication chain
//     refuses a credential scoped to none before the routes that ask. Both
//     are what the platform serves every tenant: a team's launches resolve
//     to them, as the bot named or as a child, a fragment's or a skill's
//     producer. The verdict is the tier's, not the bot's: a platform or
//     baked bot BUILT ON a team's own row is still refused to a caller who
//     may not read that row, since the guard is asked of every bundle of
//     the freeze. A rule narrower than that for the platform's overrides was a
//     boundary in name only — every account owns a personal team, of which
//     it is a bot editor — so there is none, and it is said: nothing
//     confidential belongs in a platform override
//     (docs/cloud-rest-api.md). The admin route that WRITES them stays the
//     super-admin's.
//
// What the guard keeps from a caller it refuses is the `.bot` TEXT. It keeps
// neither the digest, which is the workflow_hash on the header of every run
// of the bot, nor what a run shows: such a caller can still launch the bot by
// id where the launch gate admits them, and the prompts a run renders reach
// its launcher and every viewer of that run.
func (s *Server) botLaunchSourceReadable(r *http.Request, id auth.Identity, origin string) bool {
	if origin == "team" {
		return s.editsScopedTeamBots(r, id)
	}
	return true
}

// editsScopedTeamBots reports whether the caller may edit the bots of the
// team the request is scoped to. No scope, no team: an identity that names
// none edits none, whatever role it carries.
func (s *Server) editsScopedTeamBots(r *http.Request, id auth.Identity) bool {
	return id.TeamID != "" && s.canEditBots(r.Context(), id, id.TeamID)
}

// tierReadRefusal is a team's own row the caller may not read, met by a
// resolution that carries a read guard: the bot asked for, or a bundle it
// builds on. Only a team row is ever refused (botLaunchSourceReadable).
type tierReadRefusal struct {
	slug string // the bundle, as the team's row names it
}

func (e *tierReadRefusal) Error() string {
	return fmt.Sprintf("%q is your team's own bot: its program is readable by a bot editor, a team admin or an owner of the team", e.slug)
}

// isTierReadRefusal reports whether err is a read guard's refusal — the
// caller's standing, which a freeze passes through whole rather than wrap as
// a tier that failed.
func isTierReadRefusal(err error) bool {
	var refused *tierReadRefusal
	return errors.As(err, &refused)
}

// withTierReadGuard arms ctx so that a resolution under it materializes no
// bundle whose text the caller may not read (botLaunchSourceReadable): the
// bot itself, and in a freeze every bundle it builds on. One predicate for
// the read of a bot's program and for a launch that presents a pin, since
// only a reader can have been issued one.
func (s *Server) withTierReadGuard(ctx context.Context, r *http.Request, id auth.Identity) context.Context {
	return context.WithValue(ctx, tierReadGuardKey{}, tierReadGuard(func(hit botTierHit) error {
		if s.botLaunchSourceReadable(r, id, hit.origin) {
			return nil
		}
		return &tierReadRefusal{slug: hit.row.Slug}
	}))
}

// handleBotLaunchSource answers the program a launch naming this bot runs:
// GET /api/v1/bots/{name}/launch-source. The name resolves for the caller's
// active team through the launch's own tier decision and materialization
// (resolveBotTier, launchBotOfTier), and the digest answered is the identity
// the launch's compile computes of the same inputs (launchProgramOf,
// runview.LaunchProgramHash) — so the form built on this answer and the
// launch it then posts agree on the tier, the identity and the program by
// construction, and the launch refuses when they no longer do
// (expect_source_sha256).
func (s *Server) handleBotLaunchSource(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bots: missing name")
		return
	}
	id, _ := auth.FromContext(r.Context())
	// Every tier this answer draws on is one the caller may read: the guard
	// refuses the bot's own before anything of it is read, and each bundle
	// its freeze resolves — a fragment's producer, a child — the same way.
	ctx := s.withTierReadGuard(r.Context(), r, id)
	hit, found, err := s.resolveBotTier(ctx, id.TeamID, name, "")
	if err != nil {
		// A stored tier that did not answer is transient, as the freeze's
		// own failed tier is (launchSourceUnavailable).
		status := http.StatusInternalServerError
		var down *tierUnavailableError
		if errors.As(err, &down) {
			status = http.StatusServiceUnavailable
		}
		s.httpErrorFor(w, r, status, "bots: %s", redactAbsolutePathsInText(err.Error()))
		return
	}
	if !found {
		s.httpErrorFor(w, r, http.StatusNotFound, "bots: %q not found", name)
		return
	}
	// The caller's standing on the bot named is asked first, by the guard the
	// materialization asks again: a caller it refuses is answered the same
	// 403 whatever the row weighs — never the read admission's 503 on a large
	// one, which would tell them its size and charge its weight for a
	// refusal.
	if guard := tierReadGuardOf(ctx); guard != nil {
		var refused *tierReadRefusal
		if errors.As(guard(hit), &refused) {
			s.httpErrorFor(w, r, http.StatusForbidden, "bots: %s", launchSourceRefusal(name, refused))
			return
		}
	}
	// The reading parses the bot's files: it takes its share of the aggregate
	// bound (#2693) before any of them is materialized.
	release, admitted := s.admitDSLRead(w, r, botTierReadBytes(hit))
	if !admitted {
		return
	}
	defer release()
	lb, err := s.launchBotOfTier(ctx, id.TeamID, hit)
	if err != nil {
		var refused *tierReadRefusal
		if errors.As(err, &refused) {
			s.httpErrorFor(w, r, http.StatusForbidden, "bots: %s", launchSourceRefusal(name, refused))
			return
		}
		s.launchSourceUnavailable(w, r, name, err)
		return
	}
	defer lb.Cleanup()
	path, source, _ := s.launchProgramOf(lb)
	hash, err := runview.LaunchProgramHash(path, source, lb.BundleDir, lb.BundleEntry)
	if err != nil {
		s.launchSourceUnavailable(w, r, name, err)
		return
	}
	// The entry as the launch's compile reads it: inside the bundle
	// directory the resolution materialized (in cloud the frozen
	// collection), or the catalog file itself.
	entryPath := lb.Path
	if lb.BundleDir != "" {
		entry := lb.BundleEntry
		if entry == "" {
			entry = bundle.MainBotFile
		}
		entryPath = filepath.Join(lb.BundleDir, entry)
	}
	opened, err := openWorkflow(filepath.Base(entryPath), entryPath, []byte(lb.Source))
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "marshal error: %v", err)
		return
	}
	if opened.Unit != nil {
		// The unit was read from the launch's own materialization: no
		// workspace directory names its files (a bundle's files map answers
		// the same, parseUnitFiles).
		opened.Unit.Root = ""
	}
	presets, err := launchPresets(lb, entryPath)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "bots: read the presets of %q: %s", name, redactAbsolutePathsInText(err.Error()))
		return
	}
	s.writeJSONFor(w, r, botLaunchSourceResponse{
		BotID:       lb.BotID,
		Tier:        lb.Tier(),
		Source:      lb.Source,
		SHA256:      hash,
		Document:    opened.Document,
		Diagnostics: opened.Diagnostics,
		Unit:        opened.Unit,
		Bindable:    opened.Bindable,
		ReadRefusal: opened.ReadRefusal,
		Presets:     presets,
	})
}

// launchSourceRefusal is the 403 of a launch-program read: which bundle the
// caller may not read, why the bot asked for needs it, and what is left. The
// guard refuses every bundle a launch freezes with the bot — a fragment's or
// a skill's producer, a child — so the sentence speaks of the freeze, not of
// one kind of dependency.
func launchSourceRefusal(name string, refused *tierReadRefusal) string {
	const stillLaunchable = "you may still launch %q by id (bot_id) from the CLI, the API or MCP, which shows no program"
	if botregistry.NormalizeName(refused.slug) == botregistry.NormalizeName(name) {
		return refused.Error() + " — " + fmt.Sprintf(stillLaunchable, name)
	}
	return fmt.Sprintf("%q builds on another bundle whose program you may not read — %s — and this read answers from everything a launch of it freezes, so it is refused whole; ", name, refused.Error()) +
		fmt.Sprintf(stillLaunchable, name)
}

// launchSourceUnavailable answers a bot that resolves but not to a program a
// launch would run — the refusal a launch of it would draw ("resolve bot: …",
// "launch: …"). A tier that did not answer is transient; anything else is
// the bot as stored.
func (s *Server) launchSourceUnavailable(w http.ResponseWriter, r *http.Request, name string, err error) {
	status := http.StatusUnprocessableEntity
	if errors.Is(err, errBotSnapshotResolve) {
		status = http.StatusServiceUnavailable
	}
	s.httpErrorFor(w, r, status, "bots: %q does not resolve to a launchable program: %s", name, redactAbsolutePathsInText(err.Error()))
}

// launchPresets reads the preset set a launch applies off the
// materialization the launch compiles: nil when the program declares none
// and its bundle ships none. The entry's schema read is the registry's
// (LoadSchema), so a preset reads here as it does in the bots listing; its
// cache is keyed by path, which a materialized directory never repeats.
func launchPresets(lb *launchBot, entryPath string) (*botregistry.PresetsBlock, error) {
	entry := botregistry.Entry{Name: lb.BotID, Path: entryPath}
	if filepath.Base(entryPath) == bundle.MainBotFile {
		entry = botregistry.Entry{Name: lb.BotID, Path: filepath.Dir(entryPath), IsBundleDir: true}
	}
	if lb.BundleDir != "" {
		defer botregistry.PurgeSchemaCacheUnder(lb.BundleDir)
	}
	_, presets, err := botregistry.LoadSchema(entry)
	if err != nil {
		return nil, err
	}
	if presets == nil || len(presets.Entries) == 0 {
		return nil, nil
	}
	return presets, nil
}

// botTierReadBytes is the input size of reading a tier's bot: a stored row's
// files, a catalog bundle's workflow files, a loose catalog file alone.
func botTierReadBytes(hit botTierHit) int64 {
	if hit.origin != "catalog" {
		return mapBytes(hit.row.Files)
	}
	main := hit.entry.MainFile()
	if filepath.Base(main) == bundle.MainBotFile {
		return workflowBytesUnder(filepath.Dir(main))
	}
	if info, err := os.Stat(main); err == nil {
		return info.Size()
	}
	return 0
}
