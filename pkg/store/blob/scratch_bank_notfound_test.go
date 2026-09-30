package blob

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestOpenScratchBank_onlyNoSuchKeyIsGone: a bank read as gone refuses the
// run's resume for good. Only S3's answer for a missing key says so; a 404
// without it — a proxy's empty no-route answer, an ingress page — and the
// other errors are read again.
func TestOpenScratchBank_onlyNoSuchKeyIsGone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		ctype  string
		body   string
		gone   bool
	}{
		{"S3 NoSuchKey", 404, "application/xml", `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>`, true},
		{"an empty 404", 404, "", ``, false},
		{"an ingress page", 404, "text/html", `<html><body><h1>404 Not Found</h1></body></html>`, false},
		{"S3 NoSuchBucket", 404, "application/xml", `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchBucket</Code><Message>The specified bucket does not exist</Message></Error>`, false},
		{"503 SlowDown", 503, "application/xml", `<?xml version="1.0" encoding="UTF-8"?><Error><Code>SlowDown</Code><Message>Reduce your request rate.</Message></Error>`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.ctype != "" {
					w.Header().Set("Content-Type", tc.ctype)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
			c, err := NewS3(context.Background(), Config{Bucket: "b", Region: "us-east-1", Endpoint: srv.URL, UsePathStyle: true, AccessKeyID: "k", SecretAccessKey: "s"})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_, err = c.OpenScratchBank(ctx, "run-1")
			if err == nil {
				t.Fatal("the read succeeded — this proves nothing")
			}
			if gone := errors.Is(err, ErrArtifactNotFound); gone != tc.gone {
				t.Fatalf("read as gone=%v, want %v: %v", gone, tc.gone, err)
			}
		})
	}
}
