package github

import (
	"reflect"
	"testing"
)

// The regression: run 019f8191 built an app, then could not ship it. GitHub
// refused the push outright — "refusing to allow a GitHub App to create or
// update workflow .github/workflows/ci.yml without `workflows` permission" —
// so no CI ran, no image was built, and nothing could be deployed.
func TestRuntimePermissionsFor(t *testing.T) {
	base := RuntimeInstallationPermissions()

	t.Run("unknown grant keeps the historical baseline", func(t *testing.T) {
		// Connections stored before grants were recorded. Asking for more than
		// the installation has fails the whole mint, so absence of data must
		// mean "assume the old set", never "assume the new one".
		if got := RuntimePermissionsFor(nil); !reflect.DeepEqual(got, base) {
			t.Errorf("nil grant = %v, want the baseline %v", got, base)
		}
		if got := RuntimePermissionsFor(map[string]string{}); !reflect.DeepEqual(got, base) {
			t.Errorf("empty grant = %v, want the baseline %v", got, base)
		}
	})

	t.Run("delivery grants are used when present", func(t *testing.T) {
		granted := map[string]string{
			"contents": "write", "pull_requests": "write", "issues": "write",
			"metadata": "read", "repository_hooks": "write",
			"workflows": "write", "packages": "write",
		}
		got := RuntimePermissionsFor(granted)
		if got["workflows"] != "write" {
			t.Error("workflows must be requested when the installation granted it — without it the CI push is refused")
		}
		if got["packages"] != "write" {
			t.Error("packages must be requested when granted — needed to publish the image")
		}
		if got["contents"] != "write" {
			t.Error("the baseline must survive the intersection")
		}
	})

	t.Run("a permission the installation lacks is never requested", func(t *testing.T) {
		// This is the failure mode being prevented: GitHub 422s the ENTIRE
		// mint if any requested permission exceeds the grant, so one stale
		// assumption would break every token for that installation.
		granted := map[string]string{"contents": "write", "metadata": "read"}
		got := RuntimePermissionsFor(granted)
		for _, absent := range []string{"workflows", "packages", "pull_requests", "issues", "repository_hooks"} {
			if _, ok := got[absent]; ok {
				t.Errorf("requested %q which the installation did not grant", absent)
			}
		}
		if got["contents"] != "write" || got["metadata"] != "read" {
			t.Errorf("granted permissions dropped: %v", got)
		}
	})

	t.Run("an unrecognised grant set falls back rather than going unconstrained", func(t *testing.T) {
		// An empty result would be sent as no `permissions` key at all, which
		// GitHub reads as "the installation's FULL set" — the opposite of
		// least privilege.
		got := RuntimePermissionsFor(map[string]string{"something_else": "read"})
		if len(got) == 0 {
			t.Fatal("must never yield an empty permission map")
		}
		if !reflect.DeepEqual(got, base) {
			t.Errorf("got %v, want the baseline %v", got, base)
		}
	})
}

func TestMissingDeliveryPermissions(t *testing.T) {
	full := map[string]string{"workflows": "write", "packages": "write", "contents": "write"}
	if got := MissingDeliveryPermissions(full); len(got) != 0 {
		t.Errorf("nothing should be missing, got %v", got)
	}
	got := MissingDeliveryPermissions(map[string]string{"contents": "write"})
	want := []string{"packages", "workflows"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	// Unknown grants are not evidence of a gap — reporting one would send the
	// operator to fix a permission that may already be there.
	if got := MissingDeliveryPermissions(nil); got != nil {
		t.Errorf("unknown grant must report nothing missing, got %v", got)
	}
}

func TestBuildAppManifestDeliveryOptIn(t *testing.T) {
	plain := BuildAppManifest("n", "https://h", "https://h/cb")
	for _, p := range []string{"workflows", "packages"} {
		if _, ok := plain.DefaultPermissions[p]; ok {
			t.Errorf("%q must be opt-in, not part of the default manifest", p)
		}
	}
	withDelivery := BuildAppManifest("n", "https://h", "https://h/cb",
		AppManifestOptions{AllowAppDelivery: true})
	if withDelivery.DefaultPermissions["workflows"] != "write" ||
		withDelivery.DefaultPermissions["packages"] != "write" {
		t.Errorf("delivery opt-in did not request the grants: %v", withDelivery.DefaultPermissions)
	}
	if withDelivery.DefaultPermissions["contents"] != "write" {
		t.Error("the baseline must survive the opt-in")
	}
	if _, ok := withDelivery.DefaultPermissions["administration"]; ok {
		t.Error("delivery must not imply administration — they are separate grants")
	}
}
