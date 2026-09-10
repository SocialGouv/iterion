package boardmongo_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
)

// unreachableStore is a real Mongo board whose client is closed: every read
// fails at the driver, immediately and without a server. It is the honest
// producer for the rows below — a stub would only carry the terms the test
// remembered to give it.
func unreachableStore(t *testing.T) *boardmongo.Store {
	t.Helper()
	cli, err := mongo.Connect(options.Client().ApplyURI("mongodb://127.0.0.1:1/"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := cli.Disconnect(context.Background()); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	t.Cleanup(func() { _ = cli.Disconnect(context.Background()) })
	return boardmongo.New(cli.Database("iterion_read_failure"), "t1")
}

// A read that failed must not borrow the answer of a read that succeeded:
// "no labels on this board" and "no board config yet" are legitimate
// successes, and a transport failure wearing either of them sends the studio
// an empty label picker and the writers a board the tenant never declared.
func TestReadFailureIsNeverAnEmptySuccess(t *testing.T) {
	st := unreachableStore(t)

	if labels, err := st.AggregateLabels(); err == nil {
		t.Errorf("AggregateLabels on an unreachable store returned %v with no error — an operator reads it as 'this board has no labels'", labels)
	}
	if b, err := st.Board(); err == nil {
		t.Errorf("Board on an unreachable store returned %+v with no error — a create would file a card into a column the tenant never declared", b)
	}
}

// The callers, from the failing store rather than from a double: the REST
// route answers 5xx, and the MCP tool returns the error instead of an empty
// vocabulary the agent would act on.
func TestReadFailureReachesTheCallers(t *testing.T) {
	st := unreachableStore(t)
	mux := http.NewServeMux()
	(&native.BoardAPI{Resolve: func(*http.Request) (native.BoardStore, error) { return st, nil }}).
		RegisterRoutesWithMiddleware(mux, "/api/v1/board", nil)

	for _, path := range []string{"/api/v1/board/labels", "/api/v1/board/board"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code < 500 {
			t.Errorf("GET %s on an unreadable board answered %d %s — a failed read must not be served as content", path, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}

	caps := boardops.NewCapabilities("board.read")
	if out, err := boardops.Call(st, caps, "list_labels", json.RawMessage(`{}`)); err == nil {
		t.Errorf("list_labels on an unreadable board returned %s with no error — the agent would treat an unread vocabulary as an empty one", out)
	}
}
