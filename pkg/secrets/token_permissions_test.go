package secrets

import (
	"encoding/json"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestTokenPermissionProofBindsIdentityExpiryAndGrant(t *testing.T) {
	now := time.Now()
	permissions := map[string]string{"workflows": "write", "contents": "write"}
	p := NewTokenPermissionProof("token-one", permissions, now.Add(time.Hour))
	permissions["workflows"] = "read"
	if !p.Allows("token-one", "workflows", now) || p.Allows("token-two", "workflows", now) || p.Allows("token-one", "workflows", now.Add(time.Hour)) || p.Allows("token-one", "issues", now) {
		t.Fatal("proof must bind the actual token, returned grant and expiry without aliasing the caller's map")
	}
	if NewTokenPermissionProof("token", nil, now) != nil || NewTokenPermissionProof("token", permissions, time.Time{}) != nil {
		t.Fatal("missing provider evidence must remain unknown")
	}
	var absent *TokenPermissionProof
	if absent.Allows("token", "workflows", now) {
		t.Fatal("absent proof allowed")
	}
	for _, format := range []string{"json", "bson"} {
		t.Run(format, func(t *testing.T) {
			rec := GenericSecret{ID: "managed", ForgeTokenProof: p}
			var data []byte
			var err error
			if format == "json" {
				data, err = json.Marshal(rec)
			} else {
				data, err = bson.Marshal(rec)
			}
			if err != nil {
				t.Fatal(err)
			}
			var restored GenericSecret
			if format == "json" {
				err = json.Unmarshal(data, &restored)
			} else {
				err = bson.Unmarshal(data, &restored)
			}
			if err != nil || !restored.ForgeTokenProof.Allows("token-one", "workflows", now) {
				t.Fatalf("round trip: %v", err)
			}
		})
	}
}
