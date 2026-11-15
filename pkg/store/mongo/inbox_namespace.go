package mongo

import (
	"context"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// Incarnation rows live outside every historical cleanup collection. The
// namespace retains the original birth; public message RunID remains logical.
const colIncarnationUserMessages = "run_data_v1_user_messages"

type inboxDataNamespace struct {
	collection *mongo.Collection
	runID      string
	namespace  string
}

func (s *Store) inboxNamespace(ctx context.Context, runID string, parent *store.Run) (inboxDataNamespace, error) {
	if parent == nil {
		if err := store.CheckMissingRunDataParent(ctx, runID); err != nil {
			return inboxDataNamespace{}, err
		}
		parent = &store.Run{ID: runID, FormatVersion: int(store.LegacyValueFormat)}
	}
	if parent.ID != runID {
		return inboxDataNamespace{}, store.ErrRunDataIdentityChanged
	}
	if _, err := store.CheckRunFormatReplacement(ctx, parent, parent); err != nil {
		return inboxDataNamespace{}, err
	}
	format, err := store.NormalizeValueFormat(parent.FormatVersion)
	if err != nil {
		return inboxDataNamespace{}, err
	}
	header := store.RunDataHeader{RunID: runID, Format: format, Protocol: parent.DataProtocol, BirthID: parent.BirthID}
	if err := store.RequireRunDataStorage(header.Protocol); err != nil {
		return inboxDataNamespace{}, err
	}
	if err := store.CheckRunDataBinding(ctx, header); err != nil {
		return inboxDataNamespace{}, err
	}
	if header.Protocol == store.HistoricalRunData {
		return inboxDataNamespace{collection: s.userMessages, runID: runID}, nil
	}
	scope, err := store.ResolveIncarnationDataScope(ctx, header)
	if err != nil {
		return inboxDataNamespace{}, err
	}
	namespace, err := scope.BlobPrefix()
	if err != nil {
		return inboxDataNamespace{}, err
	}
	return inboxDataNamespace{collection: s.db.Collection(colIncarnationUserMessages), runID: runID, namespace: namespace}, nil
}

func (n inboxDataNamespace) messageID(id string) userMessageID {
	return userMessageID{RunID: n.runID, MessageID: id, Namespace: n.namespace}
}

func (n inboxDataNamespace) filter(ctx context.Context, original bson.M) bson.M {
	filter := make(bson.M, len(original)+1)
	for k, v := range original {
		filter[k] = v
	}
	if n.namespace != "" {
		filter["data_namespace"] = n.namespace
	}
	return withTenantFilter(ctx, filter)
}
