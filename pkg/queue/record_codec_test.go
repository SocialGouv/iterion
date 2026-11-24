package queue

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/valuecodec"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestExactRecordRunMessage(t *testing.T) {
	p, e := ExactRunMessageCodec()
	if e != nil {
		t.Fatal(e)
	}
	for _, resume := range []*ResumeSpec{nil, {}, {Answers: map[string]any{}, HostInputs: map[string]any{"n": json.Number("1e400")}}} {
		src := RunMessage{Vars: map[string]any{"large": json.Number("9007199254740993.0")}, Resume: resume, IRCompiled: json.RawMessage(`{"n":9007199254740993.0}`)}
		for _, pair := range []struct {
			enc func(context.Context, any) ([]byte, error)
			dec func(context.Context, []byte, any) error
		}{{p.EncodeJSON, p.DecodeJSON}, {p.EncodeBSON, p.DecodeBSON}} {
			wire, e := pair.enc(t.Context(), src)
			if e != nil {
				t.Fatal(e)
			}
			var got RunMessage
			if e = pair.dec(t.Context(), wire, &got); e != nil || !reflect.DeepEqual(src, got) {
				t.Fatal(e, got)
			}
		}
		if resume != nil {
			raw, e := p.EncodeBSON(t.Context(), src)
			if e != nil {
				t.Fatal(e)
			}
			frame, e := valuecodec.EncodeMap(t.Context(), resume.Answers)
			if e != nil {
				t.Fatal(e)
			}
			granular, e := bson.Marshal(bson.M{"answers": frame})
			if e != nil {
				t.Fatal(e)
			}
			if !reflect.DeepEqual(bson.Raw(raw).Lookup("resume", "answers"), bson.Raw(granular).Lookup("answers")) {
				t.Fatal("granular answer frame differs")
			}
		}
	}
}
