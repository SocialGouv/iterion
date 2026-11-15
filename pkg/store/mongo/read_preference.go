package mongo

import (
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

// readPreferenceWarning names a configured read preference other than
// primary, or returns "". Every store read follows the URI's preference —
// the store API has no per-call override — and the server's run-origin
// check (an expected birth compared before a request is served) reads the
// run through it: a secondary that has not replicated a delete and recreate
// yet answers with the original, and the check admits a request meant for a
// run that no longer holds its ID. A warning, not a refusal: an operator may
// accept the lag for read scaling.
func readPreferenceWarning(o *options.ClientOptions) string {
	if o == nil || o.ReadPreference == nil || o.ReadPreference.Mode() == readpref.PrimaryMode {
		return ""
	}
	return "store/mongo: the URI's readPreference is " + o.ReadPreference.Mode().String() +
		", not primary: run metadata reads (the run-origin check of expected births included) may answer from a lagging secondary"
}
