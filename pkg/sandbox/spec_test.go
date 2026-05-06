package sandbox

import "testing"

func TestModeIsValid(t *testing.T) {
	cases := []struct {
		in   Mode
		want bool
	}{
		{ModeInherit, true},
		{ModeNone, true},
		{ModeAuto, true},
		{ModeInline, true},
		{Mode("garbage"), false},
		{Mode("Auto"), false}, // case-sensitive
	}
	for _, c := range cases {
		if got := c.in.IsValid(); got != c.want {
			t.Errorf("Mode(%q).IsValid() = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestModeIsActive(t *testing.T) {
	cases := []struct {
		in   Mode
		want bool
	}{
		{ModeInherit, false},
		{ModeNone, false},
		{ModeAuto, true},
		{ModeInline, true},
	}
	for _, c := range cases {
		if got := c.in.IsActive(); got != c.want {
			t.Errorf("Mode(%q).IsActive() = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSpecValidate(t *testing.T) {
	cases := []struct {
		name    string
		spec    *Spec
		wantErr bool
	}{
		{"nil", nil, false},
		{"none", &Spec{Mode: ModeNone}, false},
		{"auto", &Spec{Mode: ModeAuto}, false},
		{"inline-with-image", &Spec{Mode: ModeInline, Image: "alpine"}, false},
		{"inline-empty", &Spec{Mode: ModeInline}, true},
		{"inline-image-and-build", &Spec{Mode: ModeInline, Image: "alpine", Build: &Build{Dockerfile: "x"}}, true},
		{"invalid-mode", &Spec{Mode: Mode("foo")}, true},
		{"invalid-network-mode", &Spec{Mode: ModeAuto, Network: &Network{Mode: NetworkMode("foo")}}, true},
		{"invalid-inherit", &Spec{Mode: ModeAuto, Network: &Network{Inherit: InheritMode("foo")}}, true},
		{"relative-workspace", &Spec{Mode: ModeAuto, WorkspaceFolder: "workspace"}, true},
		{"absolute-workspace", &Spec{Mode: ModeAuto, WorkspaceFolder: "/workspace"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.spec.Validate()
			if (err != nil) != c.wantErr {
				t.Fatalf("Validate() err = %v, wantErr = %v", err, c.wantErr)
			}
		})
	}
}
