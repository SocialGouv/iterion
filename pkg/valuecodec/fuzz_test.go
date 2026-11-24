package valuecodec_test

import (
	"bytes"
	"testing"

	"github.com/SocialGouv/iterion/pkg/valuecodec"
)

func FuzzExactFrameDecoder(f *testing.F) {
	for _, seed := range []string{`null`, `["iterion.exact-values/2"]`, `["iterion.exact-values/1",[["null",null]]]`, `["iterion.exact-values/1",[["array",[1]],["number","9007199254740993.0"]]]`, `["iterion.exact-values/1",[["object",[["a",1]]],["string","é"]]]`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		value, err := valuecodec.DecodeFrameJSON(t.Context(), data)
		if err != nil {
			return
		}
		canonical, err := valuecodec.CanonicalJSON(t.Context(), value)
		if err != nil {
			t.Fatal(err)
		}
		restored, err := valuecodec.DecodeFrameJSON(t.Context(), canonical)
		if err != nil {
			t.Fatal(err)
		}
		again, err := valuecodec.CanonicalJSON(t.Context(), restored)
		if err != nil || !bytes.Equal(canonical, again) {
			t.Fatalf("unstable canonical roundtrip: %v", err)
		}
	})
}
