// Package spec stores and judges the catalog identity a connector package
// may declare about its icon: the load-time sanitation, size cap and digest
// that let a route serve the mark without ever holding the file's bytes.
package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// EnvIconMaxBytes caps an icon file's size, sanitized bytes included
// (ITERION_CONNECTOR_ICON_MAX_BYTES — the operator's bound, never a bot's).
// Read at load; a value that is not a positive byte count REFUSES the load
// instead of falling back silently: a typo'd bound is a typo'd maturity
// level and must read as one.
const EnvIconMaxBytes = "ITERION_CONNECTOR_ICON_MAX_BYTES"

// IconMaxBytesDefault is the cap when the operator names none: an icon is
// a mark, not an asset bundle. The operator may RAISE it or remove it with
// 0 — a bound with no override is a defect (philosophy §1) — but a value
// that is not a non-negative number refuses the load: a typo'd bound must
// read as one.
const IconMaxBytesDefault = 64 << 10

// iconMaxBytes reads the operator's cap; 0 is uncapped.
func iconMaxBytes(getenv func(string) string) (int, error) {
	v := getenv(EnvIconMaxBytes)
	if v == "" {
		return IconMaxBytesDefault, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("spec: %s: %q is not a byte count (0 removes the cap)", EnvIconMaxBytes, v)
	}
	return n, nil
}

// loadIcon reads the connector's declared icon within fsys, sanitizes it
// and pins the digest the catalog's icon route serves as its ETag. A
// package that declares no icon loads with none; the catalog's monogram
// fallback covers it.
func loadIcon(fsys fs.FS, dir string, p *Package, getenv func(string) string) error {
	c := &p.Connector
	if c.Icon == "" {
		return nil
	}
	slash := strings.TrimPrefix(filepath.ToSlash(c.Icon), "./")
	if slash != strings.TrimPrefix(c.Icon, "./") || !fs.ValidPath(slash) || slash == "." {
		return fmt.Errorf("connector %q: icon %q is not a slash path within the package (io/fs takes a relative path with no leading slash and no \"..\")", c.ID, c.Icon)
	}
	cap, err := iconMaxBytes(getenv)
	if err != nil {
		return err
	}
	body, err := fs.ReadFile(fsys, path.Join(dir, slash))
	if err != nil {
		return fmt.Errorf("connector %q: read icon: %w", c.ID, err)
	}
	svg, err := sanitizeSVG(body, cap)
	if err != nil {
		return fmt.Errorf("connector %q: icon %s: %w", c.ID, c.Icon, err)
	}
	sum := sha256.Sum256(svg)
	p.IconSVG = svg
	p.IconDigest = hex.EncodeToString(sum[:])[:16]
	return nil
}

// sanitizeSVG filters an SVG document down to a passive picture: the
// elements that carry script or stylesheet content (script, foreignObject,
// style) or write attributes at render time (the SMIL animation family),
// every event-handler attribute, every reference that leaves the document,
// comments and processing directives. Style attributes survive —
// the CSP the route serves pins style-src to inline styles and 'none' for
// everything else — while a url() inside one is dropped as the external
// reference it is.
func sanitizeSVG(body []byte, cap int) ([]byte, error) {
	if cap > 0 && len(body) > cap {
		return nil, fmt.Errorf("icon is %d bytes, over the %d byte cap (%s)", len(body), cap, EnvIconMaxBytes)
	}
	dec := xml.NewDecoder(bytes.NewReader(body))
	dec.Strict = true
	var out bytes.Buffer
	enc := xml.NewEncoder(&out)
	skip := ""
	skipDepth := 0
	rootSeen := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("not valid XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if skip != "" {
				skipDepth++
				continue
			}
			switch strings.ToLower(t.Name.Local) {
			case "script", "foreignobject", "style",
				"animate", "set", "animatetransform", "animatemotion":
				skip = strings.ToLower(t.Name.Local)
				skipDepth = 0
				continue
			}
			kept := make([]xml.Attr, 0, len(t.Attr))
			for _, a := range t.Attr {
				local := strings.ToLower(a.Name.Local)
				switch {
				case local == "xmlns":
					continue
				case strings.HasPrefix(local, "on"):
					continue
				case local == "href" && !strings.HasPrefix(strings.TrimSpace(a.Value), "#"):
					continue
				case local == "style" && strings.Contains(strings.ToLower(a.Value), "url("):
					continue
				case strings.Contains(strings.ToLower(a.Value), "javascript:"):
					continue
				}
				a.Name.Space = ""
				kept = append(kept, a)
			}
			t.Attr = kept
			if !rootSeen {
				if t.Name.Local != "svg" {
					return nil, fmt.Errorf("root element is %q, not svg", t.Name.Local)
				}
				rootSeen = true
				rootNS := t.Name.Space
				t.Name.Space = ""
				if rootNS != "" {
					t.Attr = append(t.Attr, xml.Attr{
						Name:  xml.Name{Local: "xmlns"},
						Value: rootNS,
					})
				}
			} else {
				// children are emitted namespace-less: under the root's
				// xmlns they are svg elements again, which is what keeps
				// the encoder from re-declaring the namespace per element.
				t.Name.Space = ""
			}
			if err := enc.EncodeToken(t); err != nil {
				return nil, fmt.Errorf("re-encode: %w", err)
			}
		case xml.EndElement:
			if skip != "" {
				skipDepth--
				if skipDepth < 0 {
					skip = ""
				}
				continue
			}
			t.Name.Space = ""
			if err := enc.EncodeToken(t); err != nil {
				return nil, fmt.Errorf("re-encode: %w", err)
			}
		case xml.CharData:
			if skip == "" {
				if err := enc.EncodeToken(t); err != nil {
					return nil, fmt.Errorf("re-encode: %w", err)
				}
			}
		case xml.Comment, xml.ProcInst, xml.Directive:
			// dropped whole: comments have carried conditional junk for old
			// parsers, and a directive (DOCTYPE) has carried entity tricks;
			// the encoder re-emits what the document needs instead.
		}
	}
	if skip != "" {
		return nil, fmt.Errorf("<%s> never closes", skip)
	}
	if !rootSeen {
		return nil, fmt.Errorf("holds no svg root element")
	}
	if err := enc.Flush(); err != nil {
		return nil, fmt.Errorf("re-encode: %w", err)
	}
	// The re-encoding can EXPAND: a self-closing element re-emits with a
	// close tag and a tab escapes, so the sanitized bytes can exceed the
	// input the cap bounded. The served bytes are capped too (cap 0 is
	// the operator's uncapped override and bounds nothing, by choice).
	if cap > 0 && out.Len() > cap {
		return nil, fmt.Errorf("sanitized icon is %d bytes, over the %d byte cap (%s)", out.Len(), cap, EnvIconMaxBytes)
	}
	return out.Bytes(), nil
}

// SetupFile is the package's own setup instructions, read at load.
const SetupFile = "setup.md"

// SetupPollFile is the setup instructions for the package's polled
// triggers, read at load beside SetupFile: a package whose setup.md walks
// through a webhook ships the steps a poller needs on their own, so the
// polled flow shows no step it never takes.
const SetupPollFile = "setup-poll.md"

// SetupMaxBytes bounds each setup file a catalog card will carry. A setup
// guide is a page; a package shipping megabytes of it is a package whose
// author meant something else.
const SetupMaxBytes = 32 << 10

// readSetup reads setup.md and setup-poll.md when the package ships them.
func readSetup(fsys fs.FS, dir string, p *Package) error {
	var err error
	if p.Setup, err = readSetupFile(fsys, dir, SetupFile); err != nil {
		return err
	}
	p.SetupPoll, err = readSetupFile(fsys, dir, SetupPollFile)
	return err
}

// readSetupFile reads one capped setup file; nil when the package ships
// none.
func readSetupFile(fsys fs.FS, dir, name string) ([]byte, error) {
	body, err := fs.ReadFile(fsys, path.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("spec: read %s: %w", name, err)
	}
	if len(body) > SetupMaxBytes {
		return nil, fmt.Errorf("spec: %s is %d bytes, over the %d byte cap", name, len(body), SetupMaxBytes)
	}
	return body, nil
}
