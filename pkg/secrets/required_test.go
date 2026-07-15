package secrets

import "testing"

func TestUnresolvedRequired(t *testing.T) {
	cases := []struct {
		name     string
		required []string
		resolved map[string]bool
		want     []string
	}{
		{
			name:     "all resolved",
			required: []string{"forge_token", "kubeconfig"},
			resolved: map[string]bool{"forge_token": true, "kubeconfig": true},
			want:     nil,
		},
		{
			name:     "one missing",
			required: []string{"forge_token", "kubeconfig"},
			resolved: map[string]bool{"forge_token": true},
			want:     []string{"kubeconfig"},
		},
		{
			name:     "all missing, sorted",
			required: []string{"zeta", "alpha"},
			resolved: map[string]bool{},
			want:     []string{"alpha", "zeta"},
		},
		{
			name:     "blank and duplicate names ignored",
			required: []string{"  ", "forge_token", "forge_token"},
			resolved: map[string]bool{},
			want:     []string{"forge_token"},
		},
		{
			name:     "nil resolved treats everything as missing",
			required: []string{"forge_token"},
			resolved: nil,
			want:     []string{"forge_token"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := UnresolvedRequired(tc.required, tc.resolved)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestRequiredSecretsError(t *testing.T) {
	if err := RequiredSecretsError(nil, "this team/bot"); err != nil {
		t.Fatalf("expected nil for no missing, got %v", err)
	}
	err := RequiredSecretsError([]string{"forge_token"}, "this team/bot")
	if err == nil {
		t.Fatal("expected error")
	}
	want := `secret "forge_token" is declared required by the workflow but resolves to nothing for this team/bot`
	if err.Error() != want {
		t.Fatalf("got %q, want %q", err.Error(), want)
	}
	// Multiple names are joined.
	err = RequiredSecretsError([]string{"a", "b"}, "this workspace")
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); got == "" || got == want {
		t.Fatalf("unexpected joined error: %q", got)
	}
	// Empty scope falls back to a neutral phrase.
	err = RequiredSecretsError([]string{"a"}, "")
	if err == nil || err.Error() == "" {
		t.Fatal("expected non-empty error with fallback scope")
	}
}
