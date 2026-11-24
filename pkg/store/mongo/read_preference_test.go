package mongo

import (
	"bytes"
	"net/url"
	"os"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// A URI whose read preference is not primary is named; the default and an
// explicit primary are silent.
func TestReadPreferenceWarningNamesANonPrimaryPreference(t *testing.T) {
	for uri, want := range map[string]string{
		"mongodb://db.invalid/":                                   "",
		"mongodb://db.invalid/?readPreference=primary":            "",
		"mongodb://db.invalid/?readPreference=primaryPreferred":   "primaryPreferred",
		"mongodb://db.invalid/?readPreference=secondary":          "secondary",
		"mongodb://db.invalid/?readPreference=secondaryPreferred": "secondaryPreferred",
		"mongodb://db.invalid/?readPreference=nearest&w=majority": "nearest",
	} {
		got := readPreferenceWarning(options.Client().ApplyURI(uri))
		if want == "" {
			if got != "" {
				t.Errorf("%s: warning %q, want none", uri, got)
			}
			continue
		}
		if !strings.Contains(got, "readPreference is "+want+",") {
			t.Errorf("%s: warning %q, want one naming %s", uri, got, want)
		}
	}
}

// The store warns when it opens on such a URI — the operator's startup log
// carries it — and stays silent on the default. Gated on
// ITERION_TEST_MONGO_URI: New pings the primary before it returns.
func TestNewWarnsOnANonPrimaryReadPreference_Mongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping the read-preference warning at open")
	}
	open := func(t *testing.T, uri string) string {
		t.Helper()
		ctx, cancel := mongotest.Ctx(t)
		defer cancel()
		var logs bytes.Buffer
		s, err := New(ctx, Config{URI: uri, Database: "iterion_readpref_" + bsonNonce(t), Blob: newInMemoryBlob(), Logger: iterlog.New(iterlog.LevelInfo, &logs)})
		if err != nil {
			t.Fatalf("mongo New: %v", err)
		}
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = s.db.Drop(drop)
		_ = s.Close(drop)
		return logs.String()
	}
	if logs := open(t, uri); strings.Contains(logs, "readPreference is") {
		t.Fatalf("default URI logged %q", logs)
	}
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("readPreference", "primaryPreferred")
	u.RawQuery = q.Encode()
	if logs := open(t, u.String()); !strings.Contains(logs, "readPreference is primaryPreferred") {
		t.Fatalf("a primaryPreferred URI logged %q, want the warning", logs)
	}
}
