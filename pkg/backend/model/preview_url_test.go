package model

import (
	"reflect"
	"testing"
)

func TestScanPreviewURLs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []map[string]interface{}
	}{
		{
			name: "empty input",
			in:   "",
			want: nil,
		},
		{
			name: "no directive",
			in:   "starting server on :8000\nlistening...\n",
			want: nil,
		},
		{
			name: "plain url",
			in:   "[iterion] preview_url=http://localhost:8000\n",
			want: []map[string]interface{}{
				{"url": "http://localhost:8000", "source": "tool-stdout", "scope": "external"},
			},
		},
		{
			name: "with kind and scope",
			in:   "starting...\n[iterion] preview_url=http://localhost:3000 kind=dev-server scope=internal\ndone\n",
			want: []map[string]interface{}{
				{"url": "http://localhost:3000", "source": "tool-stdout", "scope": "internal", "kind": "dev-server"},
			},
		},
		{
			name: "two directives",
			in:   "[iterion] preview_url=http://a/\nfoo\n[iterion] preview_url=http://b/ kind=deploy\n",
			want: []map[string]interface{}{
				{"url": "http://a/", "source": "tool-stdout", "scope": "external"},
				{"url": "http://b/", "source": "tool-stdout", "scope": "external", "kind": "deploy"},
			},
		},
		{
			name: "ignores wrong prefix",
			in:   "iterion preview_url=http://x/\npreview_url=http://y/\n",
			want: nil,
		},
		{
			name: "ignores empty url",
			in:   "[iterion] preview_url=\n",
			want: nil,
		},
		{
			name: "ignores unknown kv pairs",
			in:   "[iterion] preview_url=http://z/ foo=bar kind=dev-server\n",
			want: []map[string]interface{}{
				{"url": "http://z/", "source": "tool-stdout", "scope": "external", "kind": "dev-server"},
			},
		},
		{
			name: "trims whitespace",
			in:   "  [iterion] preview_url=http://localhost:1/  \n",
			want: []map[string]interface{}{
				{"url": "http://localhost:1/", "source": "tool-stdout", "scope": "external"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scanPreviewURLs(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("scanPreviewURLs() = %v, want %v", got, tc.want)
			}
		})
	}
}
