//go:build darwin

package main

import "testing"

func TestMergePaths_DedupesAndPreservesOrder(t *testing.T) {
	cases := []struct {
		name string
		cur  string
		add  string
		want string
	}{
		{
			name: "empty current",
			cur:  "",
			add:  "/a:/b",
			want: "/a:/b",
		},
		{
			name: "no overlap",
			cur:  "/a:/b",
			add:  "/c:/d",
			want: "/a:/b:/c:/d",
		},
		{
			name: "full overlap",
			cur:  "/a:/b",
			add:  "/a:/b",
			want: "/a:/b",
		},
		{
			name: "partial overlap preserves cur order",
			cur:  "/a:/b:/c",
			add:  "/c:/d:/a",
			want: "/a:/b:/c:/d",
		},
		{
			name: "skip empty entries",
			cur:  "/a::/b",
			add:  "::/c",
			want: "/a:/b:/c",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergePaths(tc.cur, tc.add)
			if got != tc.want {
				t.Errorf("mergePaths(%q, %q) = %q, want %q", tc.cur, tc.add, got, tc.want)
			}
		})
	}
}
