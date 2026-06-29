package server

import (
	"reflect"
	"testing"
)

func TestGitHubOrgsFromGroups(t *testing.T) {
	got := githubOrgsFromGroups([]string{
		"socialgouv/*",
		"socialgouv/iterion",
		"dnum-socialgouv/*",
		"socialgouv/another-team", // dup org
		"",                        // ignored
		"/weird",                  // empty org → ignored
	})
	want := []string{"socialgouv", "dnum-socialgouv"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if len(githubOrgsFromGroups(nil)) != 0 {
		t.Fatal("nil groups should yield no orgs")
	}
}
