package blob

import (
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"reflect"
	"strings"
	"testing"
)

func TestValueFormatArtifactKeys(t *testing.T) {
	t.Parallel()
	for _, f := range []int{0, 1, 2} {
		k, e := artifactKeyForFormat(f, "r", "n", 4)
		if e != nil {
			t.Fatal(e)
		}
		p, e := artifactRunPrefixForFormat(f, "r")
		if e != nil {
			t.Fatal(e)
		}
		want := "artifacts/r/n/4.json"
		if f == 2 {
			want = "artifacts-v2/r/n/4.json"
		}
		if k != want || !strings.HasPrefix(k, p) {
			t.Fatal(k, p)
		}
		other, e := artifactRunPrefixForFormat(3-f, "r")
		if f != 0 && e == nil && strings.HasPrefix(k, other) {
			t.Fatal("cross format prefix", k, other)
		}
	}
	for _, f := range []int{-1, 3, 99} {
		if _, e := artifactKeyForFormat(f, "r", "n", 0); !errors.Is(e, store.ErrUnsupportedValueFormat) {
			t.Fatal(e)
		}
	}
	for _, id := range []string{"", "../other", "a/b", "a\\b"} {
		if _, e := artifactKeyForFormat(2, id, "n", 0); e == nil {
			t.Fatal("bad run accepted", id)
		}
		if _, e := artifactNodePrefixForFormat(2, "r", id); e == nil {
			t.Fatal("bad node accepted", id)
		}
	}
}
func TestValueFormatS3NamespacesPublic(t *testing.T) {
	t.Parallel()
	fake, srv := newFakeS3(t, "b")
	c, e := NewS3(t.Context(), Config{Bucket: "b", Region: "us-east-1", Endpoint: srv.URL, UsePathStyle: true, AccessKeyID: "test", SecretAccessKey: "test"})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = c.Close() })
	for _, f := range []int{1, 2} {
		for _, r := range []string{"r", "r-other"} {
			body := []byte(fmt.Sprintf("{\"format\":%d}", f))
			if e = c.PutArtifactForFormat(t.Context(), f, r, "n", 1, body); e != nil {
				t.Fatal(e)
			}
			got, e := c.GetArtifactForFormat(t.Context(), f, r, "n", 1)
			if e != nil || string(got) != string(body) {
				t.Fatal(string(got), e)
			}
			vs, e := c.ListArtifactVersionsForFormat(t.Context(), f, r, "n")
			if e != nil || !reflect.DeepEqual(vs, []int{1}) {
				t.Fatal(vs, e)
			}
		}
	}
	if got := fake.keys(); !reflect.DeepEqual(got, []string{"artifacts-v2/r-other/n/1.json", "artifacts-v2/r/n/1.json", "artifacts/r-other/n/1.json", "artifacts/r/n/1.json"}) {
		t.Fatal(got)
	}
	if e = c.DeleteRun(t.Context(), "r"); e != nil {
		t.Fatal(e)
	}
	if _, e = c.GetArtifactForFormat(t.Context(), 2, "r", "n", 1); e != nil {
		t.Fatal("legacy delete crossed namespace", e)
	}
	if e = c.DeleteRunArtifactsForFormat(t.Context(), 2, "r"); e != nil {
		t.Fatal(e)
	}
	for _, f := range []int{1, 2} {
		if _, e = c.GetArtifactForFormat(t.Context(), f, "r", "n", 1); !errors.Is(e, ErrArtifactNotFound) {
			t.Fatal(e)
		}
		if _, e = c.GetArtifactForFormat(t.Context(), f, "r-other", "n", 1); e != nil {
			t.Fatal("neighbor erased", e)
		}
	}
	before := fake.keys()
	for _, call := range []func() error{func() error { return c.PutArtifactForFormat(t.Context(), 99, "r", "n", 1, []byte("bad")) }, func() error { _, e := c.GetArtifactForFormat(t.Context(), 99, "r", "n", 1); return e }, func() error { _, e := c.ListArtifactVersionsForFormat(t.Context(), 99, "r", "n"); return e }, func() error { return c.DeleteRunArtifactsForFormat(t.Context(), 99, "r") }} {
		if e = call(); !errors.Is(e, store.ErrUnsupportedValueFormat) {
			t.Fatal(e)
		}
	}
	if !reflect.DeepEqual(before, fake.keys()) {
		t.Fatal("unknown changed objects")
	}
}
