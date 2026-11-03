// Package legal is the operator's legal surface: the documents a deployment
// publishes (legal notice, privacy policy, terms of use, accessibility
// statement), the contact address and the accessibility status, all of them
// DEPLOYMENT CONFIGURATION — never part of the bundle, never stored in the
// database (ADR-160). One bundle serves iterion.cloud and every self-hosted
// deployment, so the operator's identity, DPO and hosting provider cannot live
// in the code; and a legal text changes once a year through a reviewed
// commit, so it has no business in an editable store.
//
// This package is a leaf: pkg/config validates against it, cmd/iterion loads
// the documents through it, pkg/server serves what it loaded. The slug table
// is the one choke point every site reads — a new document is a new row here
// and nowhere else.
package legal

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Slug names one legal document. The slugs are French and readable in the
// address bar on purpose: the pages are what a French public service owes
// its visitors (LCEN, RGPD art. 13, RGAA), and /legal/mentions-legales is
// the address a visitor expects.
type Slug string

const (
	SlugLegalNotice   Slug = "mentions-legales"
	SlugPrivacyPolicy Slug = "confidentialite"
	SlugTerms         Slug = "cgu"
	SlugAccessibility Slug = "accessibilite"
)

// Slugs is the closed set of documents, in the order the links are shown.
var Slugs = []Slug{SlugLegalNotice, SlugPrivacyPolicy, SlugTerms, SlugAccessibility}

// titles are the page titles the SPA shows for each document — end-user
// text, hence French.
var titles = map[Slug]string{
	SlugLegalNotice:   "Mentions légales",
	SlugPrivacyPolicy: "Politique de confidentialité",
	SlugTerms:         "Conditions générales d'utilisation",
	SlugAccessibility: "Déclaration d'accessibilité",
}

// Title returns the user-facing title of a slug, and false for a slug that
// names no document.
func Title(slug Slug) (string, bool) {
	t, ok := titles[slug]
	return t, ok
}

// Status is the accessibility conformance status of the deployment, in the
// three states the French accessibility statement model knows (décret
// 2019-768). The zero value means "not published".
type Status string

const (
	StatusNonCompliant       Status = "non_compliant"
	StatusPartiallyCompliant Status = "partially_compliant"
	StatusCompliant          Status = "compliant"
)

// Statuses is the closed set of publishable statuses.
var Statuses = []Status{StatusNonCompliant, StatusPartiallyCompliant, StatusCompliant}

// ParseStatus accepts the empty string (not published) and the three
// statuses; anything else is refused by name.
func ParseStatus(raw string) (Status, error) {
	s := Status(strings.ToLower(strings.TrimSpace(raw)))
	switch s {
	case "", StatusNonCompliant, StatusPartiallyCompliant, StatusCompliant:
		return s, nil
	}
	return "", fmt.Errorf("accessibility status %q invalid (want one of %s, or empty)", raw, joinStatuses())
}

func joinStatuses() string {
	parts := make([]string, 0, len(Statuses))
	for _, s := range Statuses {
		parts = append(parts, string(s))
	}
	return strings.Join(parts, "|")
}

// Accessibility is the published accessibility declaration summary: the
// status, the date the statement was established (YYYY-MM-DD) and the
// conformance rate in percent when a full audit produced one.
type Accessibility struct {
	Status Status `json:"status,omitempty"`
	Date   string `json:"date,omitempty"`
	// Rate is the conformance rate in percent, as the operator wrote it
	// ("72" or "72.5"); kept as text so the page prints the figure the audit
	// stated. Empty when no full audit backs the statement.
	Rate string `json:"rate,omitempty"`
}

// Validate refuses a declaration the statement model cannot carry: an
// unknown status, a date that is not YYYY-MM-DD, a rate outside 0–100, a
// date/rate given with no status at all (a figure with no state is a
// half-declaration nobody can read), a partially compliant status with no
// rate, and a rate that contradicts its status. The RGAA statement model
// ties the two: compliant is 100 %, partially compliant is at least 50 %
// and below 100 %, non compliant is below 50 % (or no valid audit, hence no
// rate).
func (a Accessibility) Validate() error {
	if _, err := ParseStatus(string(a.Status)); err != nil {
		return err
	}
	if a.Status == "" && (a.Date != "" || a.Rate != "") {
		return errors.New("accessibility date/rate given without an accessibility status — set the status, or drop the date and rate")
	}
	if a.Date != "" {
		if _, err := time.Parse("2006-01-02", a.Date); err != nil {
			return fmt.Errorf("accessibility date %q invalid (want YYYY-MM-DD)", a.Date)
		}
	}
	if a.Rate != "" {
		f, err := strconv.ParseFloat(strings.ReplaceAll(a.Rate, ",", "."), 64)
		// Written as the in-range test negated so NaN, which compares false
		// to everything, is refused too.
		if err != nil || !(f >= 0 && f <= 100) {
			return fmt.Errorf("accessibility rate %q invalid (want a percentage between 0 and 100)", a.Rate)
		}
		if want := statusOfRate(f); want != a.Status {
			return fmt.Errorf("accessibility rate %s %% contradicts status %s (the RGAA statement model reads %s for that rate: compliant = 100 %%, partially_compliant = 50 to under 100 %%, non_compliant = under 50 %%)", a.Rate, a.Status, want)
		}
	}
	if a.Status == StatusPartiallyCompliant && a.Rate == "" {
		return errors.New("accessibility status partially_compliant needs its conformance rate — set the rate the audit measured (50 to under 100)")
	}
	return nil
}

// statusOfRate is the status the RGAA statement model gives a conformance
// rate in percent.
func statusOfRate(f float64) Status {
	switch {
	case f >= 100:
		return StatusCompliant
	case f >= 50:
		return StatusPartiallyCompliant
	default:
		return StatusNonCompliant
	}
}

// Document is one loaded legal document.
type Document struct {
	Slug     Slug   `json:"slug"`
	Title    string `json:"title"`
	Markdown string `json:"markdown"`
	// ETag is the strong validator of Markdown, so a browser revalidates a
	// page it already holds with a 304 instead of a second body.
	ETag string `json:"-"`
}

// Surface is everything the server publishes: the loaded documents (in Slugs
// order, configured ones only), the contact address and the accessibility
// declaration. The zero value publishes nothing: no route answers, no link
// is shown.
type Surface struct {
	Documents     []Document
	ContactEmail  string
	Accessibility Accessibility
}

// Document returns the loaded document for slug, if configured.
func (s Surface) Document(slug Slug) (Document, bool) {
	for _, d := range s.Documents {
		if d.Slug == slug {
			return d, true
		}
	}
	return Document{}, false
}

// Load reads the documents of dir: one markdown file per slug, named
// `<slug>.md`. A slug with no file is simply not published. Dot-entries are
// skipped — a Kubernetes ConfigMap mount carries its own `..data` and
// `..<timestamp>` entries — but any other file is refused by name: a
// `cgu.markdown` or a `mentions_legales.md` is a document the operator meant
// to publish and would otherwise vanish in silence.
//
// An empty dir string loads nothing; a dir that cannot be read is an error.
func Load(dir string) ([]Document, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("legal documents dir %s: %w", dir, err)
	}
	docs, err := LoadFS(os.DirFS(dir))
	if err != nil {
		return nil, fmt.Errorf("legal documents dir %s: %w", dir, err)
	}
	return docs, nil
}

// LoadFS is Load over an fs.FS rooted at the documents dir.
func LoadFS(fsys fs.FS) ([]Document, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	found := map[Slug]Document{}
	var stray []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || e.IsDir() {
			continue
		}
		slug, ok := slugOfFile(name)
		if !ok {
			stray = append(stray, name)
			continue
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("document %s: %w", name, err)
		}
		if strings.TrimSpace(string(body)) == "" {
			return nil, fmt.Errorf("document %s is empty — remove the file to unpublish the page, or give it a text", name)
		}
		found[slug] = newDocument(slug, string(body))
	}
	if len(stray) > 0 {
		sort.Strings(stray)
		return nil, fmt.Errorf("files that name no document: %s (want one of %s, each as <slug>.md)", strings.Join(stray, ", "), joinSlugFiles())
	}
	docs := make([]Document, 0, len(found))
	for _, slug := range Slugs {
		if d, ok := found[slug]; ok {
			docs = append(docs, d)
		}
	}
	return docs, nil
}

func newDocument(slug Slug, markdown string) Document {
	sum := sha256.Sum256([]byte(markdown))
	return Document{
		Slug:     slug,
		Title:    titles[slug],
		Markdown: markdown,
		ETag:     `"` + hex.EncodeToString(sum[:16]) + `"`,
	}
}

func slugOfFile(name string) (Slug, bool) {
	base, ok := strings.CutSuffix(name, ".md")
	if !ok {
		return "", false
	}
	slug := Slug(base)
	_, known := titles[slug]
	return slug, known
}

func joinSlugFiles() string {
	parts := make([]string, 0, len(Slugs))
	for _, s := range Slugs {
		parts = append(parts, string(s)+".md")
	}
	return strings.Join(parts, ", ")
}
