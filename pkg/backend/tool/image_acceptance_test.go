package tool

import (
	"errors"
	"strings"
	"testing"
)

// What a route's catalog states decides the acceptance under "auto" (and an
// unset hatch); "send" overrides a stated refusal, "refuse" refuses on every
// route, and any other value is an error naming the knob.
func TestImageAcceptanceFor(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		hatch  string
		stated *bool
		want   ImageAcceptance
	}{
		{"", nil, ImagesUnknown},
		{"", &yes, ImagesAccepted},
		{"", &no, ImagesRefused},
		{"auto", &no, ImagesRefused},
		{" AUTO ", nil, ImagesUnknown},
		{"send", &no, ImagesAccepted},
		{"send", nil, ImagesAccepted},
		{"refuse", &yes, ImagesRefused},
		{"refuse", nil, ImagesRefused},
	} {
		got, err := ImageAcceptanceFor(tc.stated, func(name string) string {
			if name == EnvImageAcceptance {
				return tc.hatch
			}
			return ""
		})
		if err != nil || got != tc.want {
			t.Errorf("hatch %q, stated %v: got %v (err %v), want %v", tc.hatch, tc.stated, got, err, tc.want)
		}
	}
	for _, bad := range []string{"yes", "0", "sent"} {
		_, err := ImageAcceptanceFor(&no, func(string) string { return bad })
		if err == nil || !strings.Contains(err.Error(), EnvImageAcceptance) {
			t.Errorf("hatch %q: err = %v, want an error naming %s", bad, err, EnvImageAcceptance)
		}
	}
}

// The refusal the model reads names the route, says calling again will not
// help, and tells it to get the content another way.
func TestImagesRefusedError_TellsTheModelWhatToDoInstead(t *testing.T) {
	_, err := ImageResultContent("read_image", imageResultJSON("image/png", "QUJD"),
		ImagePolicy{Route: "openai_compatible/scaleway/glm-5.3", Acceptance: ImagesRefused})
	var refused *ImagesRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want *ImagesRefusedError", err)
	}
	for _, want := range []string{`"openai_compatible/scaleway/glm-5.3" does not accept images`, "again will not help", "another way"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
}

// ImageAcceptance's hatch is one of the policy's escape hatches: it crosses
// into the sandbox with the bounds.
func TestImageAcceptanceHatchIsAPolicyEnvVar(t *testing.T) {
	for _, name := range ImagePolicyEnvVars {
		if name == EnvImageAcceptance {
			return
		}
	}
	t.Fatalf("%s is not in ImagePolicyEnvVars", EnvImageAcceptance)
}
