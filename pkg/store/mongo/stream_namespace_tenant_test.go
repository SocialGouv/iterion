package mongo

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"reflect"
	"testing"
)

func TestStreamNamespaceFilterTenantBoundary(t *testing.T) {
	t.Parallel()
	for _, strict := range []bool{false, true} {
		name := "historical"
		ns := streamDataNamespace{runID: "original"}
		if strict {
			name = "strict"
			ns.namespace = "original-birth"
			ns.tenant = "canonical"
		}
		t.Run(name, func(t *testing.T) {
			t.Run("missing-tenant-panics", func(t *testing.T) {
				defer func() {
					if recover() == nil {
						t.Fatal("missing tenant returned a query")
					}
				}()
				ns.filter(context.Background(), nil)
			})
			extra := bson.M{"seq": int64(7)}
			scoped := ns.filter(store.WithIdentity(context.Background(), "scoped", "owner"), extra)
			want := bson.M{"run_id": "original", "seq": int64(7), "tenant_id": "scoped"}
			if strict {
				want["data_namespace"] = "original-birth"
			}
			if !reflect.DeepEqual(scoped, want) {
				t.Fatalf("scoped=%v want=%v", scoped, want)
			}
			if !reflect.DeepEqual(extra, bson.M{"seq": int64(7)}) {
				t.Fatal("caller filter mutated")
			}
			privileged := ns.filter(store.WithoutTenantFilter(context.Background()), nil)
			want = bson.M{"run_id": "original"}
			if strict {
				want["tenant_id"] = "canonical"
				want["data_namespace"] = "original-birth"
			}
			if !reflect.DeepEqual(privileged, want) {
				t.Fatalf("privileged=%v want=%v", privileged, want)
			}
			doc := bson.M{"payload": "unchanged"}
			ns.stamp(doc)
			want = bson.M{"payload": "unchanged", "run_id": "original"}
			if strict {
				want["tenant_id"] = "canonical"
				want["data_namespace"] = "original-birth"
			}
			if !reflect.DeepEqual(doc, want) {
				t.Fatalf("insert=%v want=%v", doc, want)
			}
		})
	}
}
