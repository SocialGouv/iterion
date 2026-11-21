package connectors_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/connector/binding"
	"github.com/SocialGouv/iterion/pkg/connector/exec"
	"github.com/SocialGouv/iterion/pkg/connector/overlay"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// A feed publisher, as RSS 2.0 (https://www.rssboard.org/rss-specification)
// and Atom 1.0 (RFC 4287) define the document: the double serves ONE
// document at ONE URL — a 404 for any other path or query, as a static file
// server answers — and, for a protected feed, refuses a request without its
// Basic credentials. The URL is the operator's, so the witness is that the
// request reaches it byte for byte: a query string kept, a trailing slash
// kept (WordPress serves /feed/ and redirects /feed, which the guarded client
// never follows).

func init() {
	registerPoll("rss.item.published", pollDouble{cases: []pollCase{
		{
			name:   "atom-public-with-query",
			base:   "/blog/feed.xml?lang=fr",
			query:  map[string]any{},
			cred:   exec.Credential{SchemeID: "none"},
			vendor: func(t *testing.T) pollVendor { return newAtomVendor() },
		},
		{
			name:   "rss-protected-trailing-slash",
			base:   "/feed/",
			query:  map[string]any{},
			cred:   feedWitnessCred,
			vendor: func(t *testing.T) pollVendor { return newRSSVendor() },
		},
	}})
}

var feedWitnessCred = exec.Credential{SchemeID: "basic", Username: "intranet-bot-7f3a", Password: "feed-pass-witness"}

type feedEntry struct {
	// id is the guid (RSS) or the Atom id; empty for an RSS item that has
	// none.
	id, link, title, author, summary, content string
	categories                                []string
	published, updated                        time.Time
}

type feedVendor struct {
	atom          bool
	path, query   string
	protected     bool
	feedAuthor    string
	entries       []*feedEntry
	changed       string
	untouched     string
	contentHeader string
}

func feedTime(s string) time.Time {
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return at
}

func newAtomVendor() *feedVendor {
	return &feedVendor{
		atom: true, path: "/blog/feed.xml", query: "lang=fr",
		feedAuthor:    "Équipe plateforme",
		contentHeader: "application/atom+xml; charset=utf-8",
		changed:       "tag:example.org,2026:blog/pollers",
		untouched:     "tag:example.org,2026:blog/feeds-are-bounded",
		entries: []*feedEntry{
			{id: "tag:example.org,2026:blog/pollers", link: "https://example.org/blog/2026/11/pollers",
				title: "Des sources qu’on interroge", author: "Camille Durand",
				summary:    "Un poller lit une source à intervalle régulier.",
				content:    "<p>Un poller lit une source à intervalle régulier, et ne livre que ce qu’il <em>observe</em>.</p>",
				categories: []string{"sources", "pollers"},
				published:  feedTime("2026-11-17T10:00:00+01:00"), updated: feedTime("2026-11-18T09:30:00+01:00")},
			{id: "tag:example.org,2026:blog/feeds-are-bounded", link: "https://example.org/blog/2026/11/feeds-are-bounded",
				title:      "Un flux a des bornes",
				summary:    "Dix mille entrées, deux cent cinquante-six niveaux.",
				categories: []string{"feeds"},
				published:  feedTime("2026-11-16T08:15:00+01:00"), updated: feedTime("2026-11-16T08:15:00+01:00")},
		},
	}
}

func newRSSVendor() *feedVendor {
	return &feedVendor{
		path: "/feed/", protected: true,
		contentHeader: "application/rss+xml; charset=UTF-8",
		changed:       "https://intranet.example.org/?p=412",
		untouched:     "https://intranet.example.org/2026/11/maintenance/",
		entries: []*feedEntry{
			// A guid that is no permalink: the id is the guid.
			{id: "https://intranet.example.org/?p=412", link: "https://intranet.example.org/2026/11/pollers/",
				title: "Nouvelles sources : les pollers", author: "Camille Durand",
				summary:    "Les sources interrogées arrivent.",
				content:    "<p>Les sources interrogées arrivent, <strong>sans webhook</strong>.</p>",
				categories: []string{"sources", "annonces"},
				published:  feedTime("2026-11-17T10:00:00Z")},
			// No guid: the id is the link.
			{link: "https://intranet.example.org/2026/11/maintenance/",
				title: "Maintenance du 20 novembre", author: "Équipe SRE",
				summary:   "Coupure de 22 h à 23 h.",
				published: feedTime("2026-11-16T16:30:00Z")},
			// Neither guid nor link: the id is the digest of the entry.
			{title: "Rappel : revue d’accès trimestrielle",
				summary:   "Chaque responsable relit les droits de son équipe.",
				published: feedTime("2026-11-15T09:00:00Z")},
		},
	}
}

// revise edits the first entry: a new title and a new body, as an author
// fixing a post.
func (v *feedVendor) revise() (changed, untouched string) {
	e := v.entries[0]
	e.title += " (mis à jour)"
	e.content = strings.Replace(e.content, "</p>", " Mise à jour : le seen set retient l’entrée.</p>", 1)
	if v.atom {
		e.updated = e.updated.Add(20 * time.Minute)
	}
	return v.changed, v.untouched
}

func (v *feedVendor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != v.path || r.URL.RawQuery != v.query {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<html><body><h1>404 Not Found</h1></body></html>"))
		return
	}
	if v.protected {
		user, pass, ok := r.BasicAuth()
		if !ok || user != feedWitnessCred.Username || pass != feedWitnessCred.Password {
			w.Header().Set("WWW-Authenticate", `Basic realm="intranet"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}
	w.Header().Set("Content-Type", v.contentHeader)
	_, _ = w.Write(v.document())
}

func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func (v *feedVendor) document() []byte {
	var b strings.Builder
	if v.atom {
		b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
		b.WriteString(`<feed xmlns="http://www.w3.org/2005/Atom" xml:lang="fr">` + "\n")
		b.WriteString("  <title>Journal de l’équipe plateforme</title>\n")
		b.WriteString(`  <link href="https://example.org/blog/feed.xml?lang=fr" rel="self"/>` + "\n")
		b.WriteString(`  <link href="https://example.org/blog/" rel="alternate"/>` + "\n")
		b.WriteString("  <id>urn:uuid:5c0d7a2e-8f1b-4c3a-9e6d-2b7f4a1c8e90</id>\n")
		fmt.Fprintf(&b, "  <updated>%s</updated>\n", v.entries[0].updated.Format(time.RFC3339))
		fmt.Fprintf(&b, "  <author><name>%s</name></author>\n", xmlText(v.feedAuthor))
		for _, e := range v.entries {
			b.WriteString("  <entry>\n")
			fmt.Fprintf(&b, "    <title>%s</title>\n", xmlText(e.title))
			fmt.Fprintf(&b, "    <link href=%q/>\n", e.link)
			fmt.Fprintf(&b, "    <id>%s</id>\n", xmlText(e.id))
			fmt.Fprintf(&b, "    <published>%s</published>\n", e.published.Format(time.RFC3339))
			fmt.Fprintf(&b, "    <updated>%s</updated>\n", e.updated.Format(time.RFC3339))
			if e.author != "" {
				fmt.Fprintf(&b, "    <author><name>%s</name></author>\n", xmlText(e.author))
			}
			for _, c := range e.categories {
				fmt.Fprintf(&b, "    <category term=%q/>\n", c)
			}
			fmt.Fprintf(&b, "    <summary>%s</summary>\n", xmlText(e.summary))
			if e.content != "" {
				fmt.Fprintf(&b, "    <content type=\"html\">%s</content>\n", xmlText(e.content))
			}
			b.WriteString("  </entry>\n")
		}
		b.WriteString("</feed>\n")
		return []byte(b.String())
	}
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:atom="http://www.w3.org/2005/Atom">` + "\n")
	b.WriteString("<channel>\n")
	b.WriteString("  <title>Intranet — actualités</title>\n")
	b.WriteString("  <link>https://intranet.example.org/</link>\n")
	// An atom:link after the channel's link: RSS's own link must win.
	b.WriteString(`  <atom:link href="https://intranet.example.org/feed/" rel="self" type="application/rss+xml"/>` + "\n")
	b.WriteString("  <description>Les nouvelles de l’intranet</description>\n")
	b.WriteString("  <lastBuildDate>Wed, 18 Nov 2026 09:30:00 +0100</lastBuildDate>\n")
	for _, e := range v.entries {
		b.WriteString("  <item>\n")
		fmt.Fprintf(&b, "    <title>%s</title>\n", xmlText(e.title))
		if e.link != "" {
			fmt.Fprintf(&b, "    <link>%s</link>\n", xmlText(e.link))
		}
		if e.id != "" {
			fmt.Fprintf(&b, "    <guid isPermaLink=\"false\">%s</guid>\n", xmlText(e.id))
		}
		if e.author != "" {
			fmt.Fprintf(&b, "    <dc:creator><![CDATA[%s]]></dc:creator>\n", e.author)
		}
		fmt.Fprintf(&b, "    <pubDate>%s</pubDate>\n", e.published.Format(time.RFC1123Z))
		for _, c := range e.categories {
			fmt.Fprintf(&b, "    <category><![CDATA[%s]]></category>\n", c)
		}
		fmt.Fprintf(&b, "    <description><![CDATA[%s]]></description>\n", e.summary)
		if e.content != "" {
			fmt.Fprintf(&b, "    <content:encoded><![CDATA[%s]]></content:encoded>\n", e.content)
		}
		b.WriteString("  </item>\n")
	}
	b.WriteString("</channel>\n</rss>\n")
	return []byte(b.String())
}

func (v *feedVendor) checkTick(t *testing.T, tick int, _ *spec.PollCheckpoint, reqs []recordedRequest) {
	t.Helper()
	if len(reqs) != 1 {
		t.Fatalf("tick %d sent %d requests, want the one GET of the feed", tick, len(reqs))
	}
	r := reqs[0]
	want := v.path
	if v.query != "" {
		want += "?" + v.query
	}
	if r.Method != http.MethodGet || r.RequestURI != want {
		t.Errorf("tick %d: %s %s, want GET %s — the operator's feed URL exactly", tick, r.Method, r.RequestURI, want)
	}
	if accept := r.Header.Get("Accept"); !strings.Contains(accept, "application/rss+xml") || !strings.Contains(accept, "application/atom+xml") {
		t.Errorf("tick %d: Accept %q names neither feed type", tick, accept)
	}
	auth := r.Header.Get("Authorization")
	switch {
	case v.protected && !strings.HasPrefix(auth, "Basic "):
		t.Errorf("tick %d: a protected feed was asked without the connection's Basic credentials (Authorization %q)", tick, auth)
	case !v.protected && auth != "":
		t.Errorf("tick %d: a public feed was sent a credential (Authorization %q)", tick, auth)
	}
}

func (v *feedVendor) wantOutputs(int) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, e := range v.entries {
		id := e.id
		if id == "" {
			id = e.link
		}
		if id == "" {
			// Its id is a digest only the decoder computes:
			// TestAFeedEntryWithNoGuidOrLinkIsOneItemWhileItStays witnesses it.
			continue
		}
		author := e.author
		if author == "" && v.atom {
			// RFC 4287 §4.2.1: an entry with no author has the feed's.
			author = v.feedAuthor
		}
		categories := e.categories
		if categories == nil {
			categories = []string{}
		}
		want := map[string]any{
			"subject": e.title, "title": e.title, "link": e.link, "thread": id, "item_id": id,
			"author": author, "actor": author, "summary": e.summary, "content": e.content,
			"categories": categories, "labels": categories,
			"published": e.published.UTC().Format(time.RFC3339),
		}
		if v.atom {
			want["updated"] = e.updated.UTC().Format(time.RFC3339)
		} else {
			// RSS 2.0 dates an item once: its entry carries no updated, and
			// the output is unset.
			want["updated"] = nil
		}
		out[id] = want
	}
	return out
}

// TestTheRSSSampleIsWhatTheDecoderReturns: the package's recorded sample is
// the decoder's answer to the Atom publisher above, field for field — the
// preview and the load-time proof read what a poller will read.
func TestTheRSSSampleIsWhatTheDecoderReturns(t *testing.T) {
	p, err := overlay.LoadPackage("rss")
	if err != nil {
		t.Fatalf("load rss: %v", err)
	}
	trig := polledTrigger(t, p, "rss.item.published")
	op, _ := p.Operation(trig.Poll.Operation)
	srv := httptest.NewServer(newAtomVendor())
	defer srv.Close()
	e := &exec.Executor{Client: exec.MarkGuarded(srv.Client()), BaseURL: srv.URL + "/blog/feed.xml?lang=fr"}
	res, err := e.Call(context.Background(), p, op, nil, exec.Credential{SchemeID: "none"})
	if err != nil || !res.OK() {
		t.Fatalf("read the feed: %v %v", err, res.Err)
	}
	var sample spec.SampleResponse
	if err := json.Unmarshal(trig.SampleBody, &sample); err != nil {
		t.Fatalf("sample %q: %v", trig.Sample, err)
	}
	got, _ := json.MarshalIndent(res.Data, "", "  ")
	if normalize(sample.Response) != normalize(res.Data) {
		t.Fatalf("sample %q is not the decoder's answer to the publisher; the decoder answers:\n%s", trig.Sample, got)
	}
}

// TestAFeedEntryWithNoGuidOrLinkIsOneItemWhileItStays (ADR-168 §7, "an RSS
// item without guid or link is seen once while it stays in the feed"): read
// twice unchanged, it is one identity at one version, distinct from its
// neighbours'.
func TestAFeedEntryWithNoGuidOrLinkIsOneItemWhileItStays(t *testing.T) {
	p, err := overlay.LoadPackage("rss")
	if err != nil {
		t.Fatalf("load rss: %v", err)
	}
	trig := polledTrigger(t, p, "rss.item.published")
	op, _ := p.Operation(trig.Poll.Operation)
	v := newRSSVendor()
	srv := httptest.NewServer(v)
	defer srv.Close()
	e := &exec.Executor{Client: exec.MarkGuarded(srv.Client()), BaseURL: srv.URL + "/feed/"}
	read := func() map[string]string {
		res, err := e.Call(context.Background(), p, op, nil, feedWitnessCred)
		if err != nil || !res.OK() {
			t.Fatalf("read the feed: %v %v", err, res.Err)
		}
		items, err := spec.PollItems(op, trig.Poll, res.Data)
		if err != nil {
			t.Fatalf("items: %v", err)
		}
		out := map[string]string{}
		for _, it := range items {
			if link, _ := it.(map[string]any)["link"].(string); link != "" {
				continue
			}
			id, version, err := spec.PollItemIdentity(trig.Poll, res.Data, it)
			if err != nil {
				t.Fatalf("identity: %v", err)
			}
			out[id] = version
		}
		return out
	}
	first, second := read(), read()
	if len(first) != 1 {
		t.Fatalf("the feed holds one entry without guid or link, the scan read %d: %v", len(first), first)
	}
	for id, version := range first {
		if id == "" || second[id] != version {
			t.Fatalf("the entry read as %q@%q, then %v — an unchanged entry must keep one identity", id, version, second)
		}
		for _, other := range []string{v.changed, v.untouched} {
			if id == other {
				t.Fatalf("the entry without guid or link shares id %q with a neighbour", id)
			}
		}
	}
}

// TestAFeedSubscriptionNarrowsOnTheEntrysCategories: the shipped category
// filter, saved on a subscription and judged on each entry the Atom
// publisher answers, as the lane stores and re-reads the event, keeps an
// entry carrying any chosen category and drops one carrying none. What each
// entry carries is the publisher's, not the package's.
func TestAFeedSubscriptionNarrowsOnTheEntrysCategories(t *testing.T) {
	p, err := overlay.LoadPackage("rss")
	if err != nil {
		t.Fatalf("load rss: %v", err)
	}
	trig := polledTrigger(t, p, "rss.item.published")
	op, _ := p.Operation(trig.Poll.Operation)
	v := newAtomVendor()
	srv := httptest.NewServer(v)
	defer srv.Close()
	e := &exec.Executor{Client: exec.MarkGuarded(srv.Client()), BaseURL: srv.URL + "/blog/feed.xml?lang=fr"}
	res, err := e.Call(context.Background(), p, op, nil, exec.Credential{SchemeID: "none"})
	if err != nil || !res.OK() {
		t.Fatalf("read the feed: %v %v", err, res.Err)
	}
	items, err := spec.PollItems(op, trig.Poll, res.Data)
	if err != nil {
		t.Fatalf("items: %v", err)
	}
	chosen := []string{"pollers", "annonces"}
	sub, err := binding.ValidateSubscription(trigger.Subscription{
		ID: "s1", BotID: "veille", TriggerID: trig.ID, ConnectionID: "conn-feed",
		Filters: map[string][]string{"category": chosen},
	}, trig, nil)
	if err != nil {
		t.Fatalf("save a subscription narrowed on categories: %v", err)
	}
	carries := map[string][]string{}
	for _, entry := range v.entries {
		carries[entry.id] = entry.categories
	}
	judged := 0
	for _, item := range items {
		id, _, err := spec.PollItemIdentity(trig.Poll, res.Data, item)
		if err != nil {
			t.Fatalf("identity: %v", err)
		}
		categories, known := carries[id]
		if !known {
			t.Fatalf("the scan read entry %q, which the publisher does not serve", id)
		}
		stored, err := json.Marshal(map[string]any{"payload": item})
		if err != nil {
			t.Fatal(err)
		}
		ev, err := binding.DecodeEvent(stored)
		if err != nil {
			t.Fatalf("entry %q: %v", id, err)
		}
		verdict, err := binding.Judge(context.Background(), sub, trig, ev)
		if err != nil {
			t.Fatalf("entry %q: %v", id, err)
		}
		want := false
		for _, c := range categories {
			want = want || slices.Contains(chosen, c)
		}
		if verdict.Refusal != "" || verdict.Matched != want {
			t.Errorf("entry %q carrying %q: verdict %+v, want matched=%v under the chosen %q", id, categories, verdict, want, chosen)
		}
		judged++
	}
	if judged != len(v.entries) {
		t.Fatalf("judged %d entries, the publisher serves %d", judged, len(v.entries))
	}
}

func polledTrigger(t *testing.T, p *spec.Package, id string) spec.Trigger {
	t.Helper()
	for _, trig := range p.Triggers {
		if trig.ID == id && trig.Polled() {
			return trig
		}
	}
	t.Fatalf("%s declares no polled trigger %q", p.Connector.ID, id)
	return spec.Trigger{}
}
