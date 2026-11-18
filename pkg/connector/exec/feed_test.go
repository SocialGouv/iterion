package exec_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// rssFeed is an RSS 2.0 channel the way feeds are served: an HTML entity
// unescaped in a description, a dc:creator instead of an author, a guid,
// a one-digit day in an RFC 822 date, content:encoded beside the summary.
const rssFeed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:content="http://purl.org/rss/1.0/modules/content/">
<channel>
  <title>iterion releases</title>
  <link>https://forge.example/iterion</link>
  <lastBuildDate>Tue, 18 Nov 2026 10:00:00 +0000</lastBuildDate>
  <item>
    <title>v7.12.0</title>
    <link>https://forge.example/iterion/releases/v7.12.0</link>
    <guid isPermaLink="false">tag:forge.example,2026:v7.12.0</guid>
    <pubDate>Tue, 8 Nov 2026 09:30:00 +0100</pubDate>
    <dc:creator>devthejo</dc:creator>
    <description>Pools&nbsp;identify a pledge by its key.</description>
    <content:encoded><![CDATA[<p>Two keys of one lender share a pool.</p>]]></content:encoded>
    <category>release</category>
    <category> credpool </category>
  </item>
  <item>
    <title>no guid</title>
    <link>https://forge.example/iterion/releases/v7.11.1</link>
  </item>
  <item>
    <title>no guid, no link</title>
  </item>
</channel>
</rss>`

// atomFeed is an Atom 1.0 feed: an alternate link among others, an xhtml
// content whose markup must survive, a published and an updated instant.
const atomFeed = `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>iterion</title>
  <link rel="self" href="https://forge.example/atom.xml"/>
  <link href="https://forge.example/"/>
  <updated>2026-11-18T10:00:00+01:00</updated>
  <entry>
    <id>urn:uuid:60a76c80-d399-11d9-b93C-0003939e0af6</id>
    <title>Sources L3</title>
    <link rel="edit" href="https://forge.example/edit/1"/>
    <link rel="alternate" href="https://forge.example/l3"/>
    <published>2026-11-17T09:00:00Z</published>
    <updated>2026-11-18T09:00:00Z</updated>
    <summary>Seven push sources.</summary>
    <content type="xhtml"><div xmlns="http://www.w3.org/1999/xhtml"><p>Seven <b>push</b> sources.</p></div></content>
    <author><name>Jo</name></author>
    <category term="sources"/>
  </entry>
</feed>`

func feedOp() spec.Operation {
	return spec.Operation{
		ID: "probe.feed.read", Resource: "feed", Verb: "read",
		HTTP:    spec.HTTPBinding{Method: "GET", Path: "/feed", ResponseFormat: spec.ResponseFeed},
		Results: []spec.ResultCase{{Status: 200}},
		Effect:  spec.EffectRead, Deterministic: true,
	}
}

// callFeed serves body as the answer to a feed operation, through the real
// executor, and returns the decoded answer and the Accept the vendor saw.
func callFeed(t *testing.T, contentType, body string) (any, string, error) {
	t.Helper()
	var accept string
	e, pkg, done := run(t, func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write([]byte(body))
	})
	defer done()
	op := feedOp()
	pkg.Ops[0].Operations = append(pkg.Ops[0].Operations, op)
	res, err := e.Call(context.Background(), pkg, op, nil, creds())
	if err == nil && res.Err != nil {
		// A vendor's answer that fails is reported on the result, the
		// executor's own refusals on err; a feed test reads either.
		err = res.Err
	}
	return res.Data, accept, err
}

func feedItems(t *testing.T, data any) []map[string]any {
	t.Helper()
	m, ok := data.(map[string]any)
	if !ok {
		t.Fatalf("the decoded feed is %T, not the normalized object", data)
	}
	raw, ok := m["items"].([]any)
	if !ok {
		t.Fatalf("items = %T", m["items"])
	}
	out := make([]map[string]any, len(raw))
	for i, it := range raw {
		out[i] = it.(map[string]any)
	}
	return out
}

// TestAFeedOperationReadsRSSThroughTheExecutor: the operation asks for a
// feed, and the answer reaches the caller in the normalized shape, with each
// fallback of the identity exercised.
func TestAFeedOperationReadsRSSThroughTheExecutor(t *testing.T) {
	data, accept, err := callFeed(t, "application/rss+xml", rssFeed)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(accept, "application/rss+xml") || !strings.Contains(accept, "application/atom+xml") {
		t.Fatalf("Accept = %q, want the feed media types", accept)
	}
	items := feedItems(t, data)
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	first := items[0]
	for k, want := range map[string]string{
		"id":        "tag:forge.example,2026:v7.12.0",
		"title":     "v7.12.0",
		"link":      "https://forge.example/iterion/releases/v7.12.0",
		"published": "2026-11-08T08:30:00Z",
		"author":    "devthejo",
		"summary":   "Pools identify a pledge by its key.",
		"content":   "<p>Two keys of one lender share a pool.</p>",
	} {
		if first[k] != want {
			t.Errorf("%s = %q, want %q", k, first[k], want)
		}
	}
	if cats, _ := first["categories"].([]any); len(cats) != 2 || cats[1] != "credpool" {
		t.Errorf("categories = %v, want the two, trimmed", first["categories"])
	}
	if v, _ := first["version"].(string); !strings.HasPrefix(v, "sha256:") {
		t.Errorf("version = %v, want the content digest", first["version"])
	}
	// The id falls back to the link, then to the content digest.
	if items[1]["id"] != "https://forge.example/iterion/releases/v7.11.1" {
		t.Errorf("an item with no guid has id %v, want its link", items[1]["id"])
	}
	if items[2]["id"] != items[2]["version"] {
		t.Errorf("an item with no guid and no link has id %v, want its digest %v", items[2]["id"], items[2]["version"])
	}
}

// TestAFeedOperationReadsAtom: Atom's id, its alternate link among others,
// and an xhtml content whose markup a chardata read would have dropped.
func TestAFeedOperationReadsAtom(t *testing.T) {
	data, _, err := callFeed(t, "application/atom+xml", atomFeed)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if feed := data.(map[string]any); feed["link"] != "https://forge.example/" || feed["updated"] != "2026-11-18T09:00:00Z" {
		t.Errorf("feed = link %v updated %v", feed["link"], feed["updated"])
	}
	e := feedItems(t, data)[0]
	if e["id"] != "urn:uuid:60a76c80-d399-11d9-b93C-0003939e0af6" || e["link"] != "https://forge.example/l3" {
		t.Errorf("entry id %v link %v", e["id"], e["link"])
	}
	if c, _ := e["content"].(string); !strings.Contains(c, "<b>push</b>") {
		t.Errorf("content = %q, want the xhtml markup kept", c)
	}
	if e["published"] != "2026-11-17T09:00:00Z" || e["updated"] != "2026-11-18T09:00:00Z" || e["author"] != "Jo" {
		t.Errorf("published %v updated %v author %v", e["published"], e["updated"], e["author"])
	}
}

// TestAFeedRevisionFollowsItsContent: an edited entry is a new revision, an
// unchanged one is not — the seen set's whole contract.
func TestAFeedRevisionFollowsItsContent(t *testing.T) {
	a, _, err := callFeed(t, "application/rss+xml", rssFeed)
	if err != nil {
		t.Fatal(err)
	}
	same, _, _ := callFeed(t, "application/rss+xml", rssFeed)
	edited, _, _ := callFeed(t, "application/rss+xml", strings.Replace(rssFeed, "by its key", "by the key it lends", 1))
	v := func(d any) any { return feedItems(t, d)[0]["version"] }
	if v(a) != v(same) {
		t.Fatal("the same entry read twice has two revisions")
	}
	if v(a) == v(edited) {
		t.Fatal("an edited entry kept its revision — it would never be seen again")
	}
	if feedItems(t, a)[0]["id"] != feedItems(t, edited)[0]["id"] {
		t.Fatal("an edit changed the identity of an entry that has a guid")
	}
}

// TestAFeedInALatinCharsetIsTranscoded: the single-byte charsets feeds still
// declare are read, and windows-1252's punctuation block is not Latin-1's.
func TestAFeedInALatinCharsetIsTranscoded(t *testing.T) {
	doc := func(charset string, title []byte) string {
		return `<?xml version="1.0" encoding="` + charset + `"?><rss version="2.0"><channel><item><title>` +
			string(title) + `</title><guid>g</guid></item></channel></rss>`
	}
	for _, tc := range []struct {
		charset string
		raw     []byte
		want    string
	}{
		{"ISO-8859-1", []byte{'R', 0xe9, 's', 'u', 'm', 0xe9}, "Résumé"},
		{"windows-1252", []byte{0x93, 'q', 'u', 'o', 't', 'e', 0x94, ' ', 0x80}, "“quote” €"},
		{"us-ascii", []byte("plain"), "plain"},
	} {
		data, _, err := callFeed(t, "application/rss+xml", doc(tc.charset, tc.raw))
		if err != nil {
			t.Fatalf("%s: %v", tc.charset, err)
		}
		if got := feedItems(t, data)[0]["title"]; got != tc.want {
			t.Errorf("%s: title = %q, want %q", tc.charset, got, tc.want)
		}
	}
}

// TestANonFeedAnswerIsAnErrorNamingWhy: a document the decoder cannot read
// is a failed call that names the cause, never an empty list a poller would
// take for "nothing new".
func TestANonFeedAnswerIsAnErrorNamingWhy(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"JSON", `{"items":[]}`, "not an RSS 2.0 or Atom 1.0 feed"},
		{"RSS 1.0", `<?xml version="1.0"?><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"></rdf:RDF>`, "neither RSS 2.0"},
		{"RSS 0.91", `<rss version="0.91"><channel></channel></rss>`, `"0.91" is not 2.x`},
		{"a feed element outside Atom", `<feed><entry/></feed>`, "neither RSS 2.0"},
		{"an unknown charset", `<?xml version="1.0" encoding="koi8-r"?><rss version="2.0"/>`, `charset "koi8-r"`},
		{"truncated", `<rss version="2.0"><channel><item><title>cut`, "decode the RSS document"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := callFeed(t, "application/xml", tc.body)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

// TestAFeedDeclaresNoEntityItCanExpand: a DTD's internal entity is never
// expanded — the classic amplification never reaches the decoded answer.
func TestAFeedDeclaresNoEntityItCanExpand(t *testing.T) {
	body := `<?xml version="1.0"?><!DOCTYPE rss [<!ENTITY a "AAAAAAAAAA"><!ENTITY b "&a;&a;&a;&a;&a;&a;&a;&a;">]>` +
		`<rss version="2.0"><channel><item><title>&b;</title><guid>g</guid></item></channel></rss>`
	data, _, err := callFeed(t, "application/rss+xml", body)
	if err == nil {
		if title := feedItems(t, data)[0]["title"]; strings.Contains(title.(string), "AAAA") {
			t.Fatalf("a DTD entity was expanded into %q", title)
		}
	}
}
