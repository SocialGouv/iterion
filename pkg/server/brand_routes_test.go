package server

import (
	"bytes"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/brand"
)

func brandGet(name, ifNoneMatch string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/brand/"+name, nil)
	r.SetPathValue("file", name)
	if ifNoneMatch != "" {
		r.Header.Set("If-None-Match", ifNoneMatch)
	}
	w := httptest.NewRecorder()
	handleBrandAsset(w, r)
	return w
}

// The /brand/ route is the one URL the docs and the studio's download links
// hand to operators for the uploads iterion cannot do itself.
func TestBrandAsset_ServesBothVariants(t *testing.T) {
	for _, v := range []brand.Variant{brand.VariantPlain, brand.VariantCircle} {
		w := brandGet(v.Filename(), "")
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/png" {
			t.Fatalf("%s: code=%d type=%q", v, w.Code, w.Header().Get("Content-Type"))
		}
		if _, err := png.DecodeConfig(bytes.NewReader(w.Body.Bytes())); err != nil {
			t.Errorf("%s: body is not a PNG: %v", v, err)
		}
		if !bytes.Equal(w.Body.Bytes(), brand.BotAvatar(v)) {
			t.Errorf("%s: served bytes differ from the embedded asset", v)
		}
		etag := w.Header().Get("ETag")
		if etag == "" || w.Header().Get("Cache-Control") == "" {
			t.Errorf("%s: missing ETag/Cache-Control (%q / %q)", v, etag, w.Header().Get("Cache-Control"))
		}
		if again := brandGet(v.Filename(), etag); again.Code != http.StatusNotModified {
			t.Errorf("%s: conditional GET with the ETag = %d, want 304", v, again.Code)
		}
	}
	if !isPublicPath("/brand/" + brand.VariantPlain.Filename()) {
		t.Error("/brand/ must be reachable without a session: the link is followed from a GitHub settings tab")
	}
}

func TestBrandAsset_UnknownFileIs404(t *testing.T) {
	for _, name := range []string{"logo.png", "../etc/passwd", ""} {
		if w := brandGet(name, ""); w.Code != http.StatusNotFound {
			t.Errorf("%q: code=%d, want 404", name, w.Code)
		}
	}
}
