package natsconfig

import "testing"

func TestNATSSubjectPermissionsCoverWholeLanguages(t *testing.T) {
	for _, tc := range []struct {
		name       string
		permission SubjectPermissions
		protected  []string
		allowed    bool
	}{
		{"unrestricted", SubjectPermissions{}, []string{"$JS.API.>"}, true},
		{"empty configured list", SubjectPermissions{Allow: []string{}}, []string{"$JS.API.>"}, true},
		{"explicit exclusion", SubjectPermissions{Deny: []string{">"}}, []string{"$JS.API.>"}, false},
		{"unrelated grant", SubjectPermissions{Allow: []string{"safe.>"}}, []string{"$JS.API.>"}, false},
		{"deny precedence", SubjectPermissions{Allow: []string{">"}, Deny: []string{"$JS.>"}}, []string{"$JS.API.>"}, false},
		{"hole below wildcard", SubjectPermissions{Allow: []string{"$JS.API.>"}, Deny: []string{"$JS.API.CONSUMER.>"}}, []string{"$JS.API.>"}, true},
		{"one token required", SubjectPermissions{Allow: []string{"a.>"}}, []string{"a"}, false},
		{"union covers tail", SubjectPermissions{Allow: []string{"a.>"}, Deny: []string{"a.*", "a.*.>"}}, []string{"a.>"}, false},
		{"literal denies leave a hole", SubjectPermissions{Allow: []string{"a.*"}, Deny: []string{"a.x", "a.y"}}, []string{"a.>"}, true},
		{"exact depth", SubjectPermissions{Allow: []string{"a.*.c"}, Deny: []string{"a.b.>"}}, []string{"a.*.*"}, true},
		{"several protected families", SubjectPermissions{Allow: []string{"$KV.locks.>"}}, []string{"$JS.API.>", "$KV.locks.>"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			witness, allowed, err := tc.permission.Intersects(tc.protected)
			if err != nil || allowed != tc.allowed {
				t.Fatalf("allowed=%t, want=%t, error=%v", allowed, tc.allowed, err)
			}
			if allowed {
				matches, err := tc.permission.Allows(witness)
				if err != nil || !matches {
					t.Fatalf("reported witness is not permitted: %v", err)
				}
			}
		})
	}
}

func TestNATSSubjectPermissionsRefuseUnsupportedPatterns(t *testing.T) {
	for _, pattern := range []string{"", "a..b", "a.>.b", "a.b*", "a.**", "a queue", "a\x00b"} {
		if _, _, err := (SubjectPermissions{Allow: []string{pattern}}).Intersects([]string{">"}); err == nil {
			t.Fatalf("unsupported pattern %q accepted", pattern)
		}
	}
}
