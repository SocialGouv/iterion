package docsguard

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// A stored bearer token is its unsalted SHA-256 digest (ADR-156): every
// input to auth.HashRefreshToken is CSPRNG output of at least 256 bits, so a
// salt defends nothing and would break the unique token_hash lookup. A page
// or comment that calls the digest salted is a claim a qualification file
// copies and a reviewer then holds the code to. This guard keeps the prose and
// the code saying the same thing, in both directions:
//
//   - no page or Go source that describes a stored bearer token claims a
//     salted hash — the word the tree uses is "unsalted";
//   - the reference pages still state the unsalted digest explicitly, so the
//     claim cannot be quietly dropped either;
//   - HashRefreshToken is still the bare digest, so "unsalted" is a fact read
//     from the function and not a sentence about it. Whoever adds a salt or a
//     key there owes ADR-156 and these pages the matching change.
//
// The Go sources it reads are not a list: they are every non-test file under
// pkg, internal and cmd that names the primitive or the field it fills
// (tokenHashProseMarkers), so a new minting or verifying site is in scope the
// day it calls HashRefreshToken, and a file about another hash is not.
// pkg/auth/password.go is the boundary case: argon2id with a per-hash salt is
// the truth there; it stays out because it names no token hash, not because
// of its wording.
//
// A claim is read over the words of a file joined across line breaks, Go
// comment markers dropped (tokenHashSaltedClaims), so a sentence wrapped over
// two lines is still the sentence it is; "hash" and "digest" are the same
// noun to it.
//
// Dated records are out of reach: docs/adr/076 keeps its sentence under a
// dated addendum, and "salted" said of an idempotency key (docs/webhooks.md,
// pkg/server) is another sense of the word, outside the matcher's form.
var tokenHashSaltedClaim = regexp.MustCompile(`(?i)\bsalted\s+(?:(?:hex|sha-?256)\s+){0,2}(?:hash|digest)|\b(?:hash|digest)(?:e?s)?\b[^.]{0,40}\b(?:is|are)\s+salted\b`)

// tokenHashProsePages describe a stored bearer token and must read "unsalted".
var tokenHashProsePages = []string{
	"docs/secrets-reference.md",
	"docs/webhooks.md",
	"docs/diagrams/data-model-knowledge.md",
	"docs/diagrams/data-model-forge-bots.md",
	"docs/adr/156-bearer-token-hashing-doctrine.md",
}

// tokenHashProseRoots are the Go trees walked for sources that describe a
// stored bearer token; tokenHashProseSkippedDirs are not Go sources of ours.
var (
	tokenHashProseRoots       = []string{"pkg", "internal", "cmd"}
	tokenHashProseSkippedDirs = []string{"testdata", "vendor", "node_modules"}
)

// tokenHashProseMarkers name the primitive and the field it fills: a source
// that contains one describes a stored bearer token and is read by the guard.
var tokenHashProseMarkers = []string{"HashRefreshToken", "TokenHash", "token_hash"}

// tokenHashProseWitnesses must come out of the walk: the primitive itself,
// the four token packages, and the two callers that hash a token outside
// them (the forge-provisioned webhook token, the invitation lookup). A witness missing
// means the scope detection broke, not that the tree is clean.
var tokenHashProseWitnesses = []string{
	"pkg/auth/refresh.go",
	"pkg/auth/password_reset.go",
	"pkg/pat/pat.go",
	"pkg/webhooks/token.go",
	"pkg/configshare/token.go",
	"pkg/identity/types.go",
	"pkg/forge/orchestrator.go",
	"pkg/server/auth_teams.go",
}

// tokenHashProseBoundary is the file where "salted hash" is the truth
// (argon2id for passwords): the guard must leave it out of its scope, since
// the matcher would refuse the correct sentence there.
const tokenHashProseBoundary = "pkg/auth/password.go"

// tokenHashDigestBody is HashRefreshToken as the doctrine states it: the hex
// of one SHA-256 over the plaintext, nothing mixed in.
var tokenHashDigestBody = regexp.MustCompile(`func HashRefreshToken\(token string\) string \{\n\tsum := sha256\.Sum256\(\[\]byte\(token\)\)\n\treturn hex\.EncodeToString\(sum\[:\]\)\n\}`)

func TestStoredBearerTokenProseSaysUnsaltedAndTheCodeAgrees(t *testing.T) {
	root := filepath.Join("..", "..")

	for _, claim := range []string{
		"stored only as a salted hash",
		"The hash on disk is salted; verification",
		"// stored only as a salted\n// hash + last4",
		"the stored value is a salted SHA-256 digest of the token",
		"token hashes are\nsalted before storage",
	} {
		if len(tokenHashSaltedClaims(claim)) == 0 {
			t.Fatalf("the matcher no longer reads %q, a claim it exists to refuse — every file below is unchecked", claim)
		}
	}
	for _, fixed := range []string{
		"only the unsalted SHA-256 hash remains",
		"// the unsalted\n// SHA-256 digest of the token",
		"the idempotency key is then salted with the MR",
	} {
		if got := tokenHashSaltedClaims(fixed); len(got) != 0 {
			t.Fatalf("the matcher refuses %q in %q, the wording the tree is meant to use — the guard would fail the fixed tree", got[0].text, fixed)
		}
	}
	if got := tokenHashSaltedClaims("one\n\n// two salted\n// hash"); len(got) != 1 || got[0].line != 3 {
		t.Fatalf("a claim wrapped over lines 3-4 is reported as %+v, not at line 3 — a finding would point at the wrong line", got)
	}

	refresh := forgeProseSource(t, "pkg", "auth", "refresh.go")
	if !tokenHashDigestBody.MatchString(refresh) {
		t.Errorf("pkg/auth/refresh.go: HashRefreshToken is no longer the bare hex SHA-256 of the plaintext\n\tADR-156 and the pages below state an UNSALTED digest; a salt or a key there makes them false and breaks the unique token_hash lookup — change the record and the prose with the function, or keep the digest")
	}

	if len(tokenHashSaltedClaims("passwords are stored as an argon2id salted hash")) == 0 {
		t.Fatalf("the matcher no longer reads the password sentence — the boundary below is untested")
	}

	sources := tokenHashProseSources(t, root)
	for _, w := range tokenHashProseWitnesses {
		if !slices.Contains(sources, w) {
			t.Fatalf("%s is not in the guard's scope — it hashes a stored bearer token, so the walk or the markers no longer follow the primitive (tokenHashProseMarkers)", w)
		}
	}
	if slices.Contains(sources, tokenHashProseBoundary) {
		t.Fatalf("%s is in the guard's scope — it describes argon2id passwords, where a salted hash is the truth; a token-hash marker there means a token hashes in the password file, and the guard would refuse the correct sentence about passwords", tokenHashProseBoundary)
	}

	files := append(slices.Clone(tokenHashProsePages), sources...)
	for _, rel := range files {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("%s: %v — a guard's subject moved; move the guard with it rather than dropping the claim", rel, err)
		}
		for _, c := range tokenHashSaltedClaims(string(raw)) {
			t.Errorf("%s:%d: %q\n\tclaims a salted hash for a stored bearer token; the digest is unsalted (ADR-156) — write \"unsalted SHA-256 hash\" and say why (the token is ≥ 256 random bits)", rel, c.line, c.text)
		}
	}

	for _, page := range tokenHashProsePages {
		if !strings.Contains(forgeProseFlat(forgeProseSource(t, strings.Split(page, "/")...)), "unsalted") {
			t.Errorf("%s no longer states that the stored token hash is unsalted — the page describes a stored bearer token and owes the reader the fact ADR-156 records", page)
		}
	}
}

// tokenHashClaim is one salted-hash claim: the words that matched and the
// line of the file where they start.
type tokenHashClaim struct {
	line int
	text string
}

// tokenHashSaltedClaims returns every tokenHashSaltedClaim in src. The words
// of src are joined with single spaces across line breaks, a leading "//"
// dropped from each, so a claim wrapped over two lines of prose or of a Go
// comment matches as one sentence; each match reports the line of its first
// word.
func tokenHashSaltedClaims(src string) []tokenHashClaim {
	var flat strings.Builder
	var starts, lines []int
	for i, line := range strings.Split(src, "\n") {
		for _, w := range strings.Fields(line) {
			if w = strings.TrimPrefix(w, "//"); w == "" {
				continue
			}
			if flat.Len() > 0 {
				flat.WriteByte(' ')
			}
			starts = append(starts, flat.Len())
			lines = append(lines, i+1)
			flat.WriteString(w)
		}
	}
	text := flat.String()
	var out []tokenHashClaim
	for _, m := range tokenHashSaltedClaim.FindAllStringIndex(text, -1) {
		word := sort.SearchInts(starts, m[0]+1) - 1
		out = append(out, tokenHashClaim{line: lines[word], text: text[m[0]:m[1]]})
	}
	return out
}

// tokenHashProseSources returns, relative to root, every non-test Go source
// under tokenHashProseRoots that contains a tokenHashProseMarkers entry.
func tokenHashProseSources(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	for _, tree := range tokenHashProseRoots {
		err := filepath.WalkDir(filepath.Join(root, tree), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if slices.Contains(tokenHashProseSkippedDirs, d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, marker := range tokenHashProseMarkers {
				if strings.Contains(string(raw), marker) {
					rel, err := filepath.Rel(root, path)
					if err != nil {
						return err
					}
					out = append(out, filepath.ToSlash(rel))
					break
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("%s: %v — a guard's subject moved; move the guard with it rather than dropping the claim", tree, err)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no Go source names a stored bearer token (%v) — the comments the guard watches would all go unchecked", tokenHashProseMarkers)
	}
	return out
}
