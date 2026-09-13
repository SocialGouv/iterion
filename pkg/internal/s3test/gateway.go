// Package s3test provides the local HTTP object gateway shared by blob and
// executable compatibility tests. Clients use their real AWS SDK/S3 code.
package s3test

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// Gateway is a minimal in-memory S3 gateway: PUT/GET object,
// HEAD bucket, ListObjectsV2 and the batch DeleteObjects POST.
type Gateway struct {
	mu      sync.Mutex
	bucket  string
	objects map[string][]byte
	// putContentTypes records the Content-Type seen per key.
	putContentTypes map[string]string
}

func New(t *testing.T, bucket string) (*Gateway, *httptest.Server) {
	t.Helper()
	f := &Gateway{bucket: bucket, objects: map[string][]byte{}, putContentTypes: map[string]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *Gateway) Keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.objects))
	for k := range f.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// splitPath maps a path-style request onto (bucket, key).
func (f *Gateway) splitPath(p string) (string, string) {
	trimmed := strings.TrimPrefix(p, "/")
	bucket, key, _ := strings.Cut(trimmed, "/")
	return bucket, key
}

func (f *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bucket, key := f.splitPath(r.URL.Path)
	if bucket != f.bucket {
		writeS3Error(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	switch {
	case r.Method == http.MethodHead && key == "":
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPost && r.URL.Query().Has("delete"):
		f.handleBatchDelete(w, r)
	case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
		f.handleList(w, r)
	case r.Method == http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeS3Error(w, http.StatusBadRequest, "IncompleteBody")
			return
		}
		f.mu.Lock()
		f.objects[key] = body
		f.putContentTypes[key] = r.Header.Get("Content-Type")
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet:
		f.mu.Lock()
		body, ok := f.objects[key]
		contentType := f.putContentTypes[key]
		f.mu.Unlock()
		if !ok {
			writeS3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(body)
	case r.Method == http.MethodDelete:
		f.mu.Lock()
		delete(f.objects, key)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

func (f *Gateway) handleList(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	type object struct {
		Key  string `xml:"Key"`
		Size int64  `xml:"Size"`
	}
	type result struct {
		XMLName     xml.Name `xml:"ListBucketResult"`
		Name        string   `xml:"Name"`
		Prefix      string   `xml:"Prefix"`
		KeyCount    int      `xml:"KeyCount"`
		IsTruncated bool     `xml:"IsTruncated"`
		Contents    []object `xml:"Contents"`
	}
	res := result{Name: f.bucket, Prefix: prefix}
	for _, k := range f.Keys() {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		f.mu.Lock()
		size := int64(len(f.objects[k]))
		f.mu.Unlock()
		res.Contents = append(res.Contents, object{Key: k, Size: size})
	}
	res.KeyCount = len(res.Contents)
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(res)
}

func (f *Gateway) handleBatchDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		XMLName xml.Name `xml:"Delete"`
		Objects []struct {
			Key string `xml:"Key"`
		} `xml:"Object"`
	}
	body, _ := io.ReadAll(r.Body)
	if err := xml.Unmarshal(body, &req); err != nil {
		writeS3Error(w, http.StatusBadRequest, "MalformedXML")
		return
	}
	f.mu.Lock()
	for _, o := range req.Objects {
		delete(f.objects, o.Key)
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></DeleteResult>`))
}

func writeS3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
}

// Snapshot returns detached object bodies for byte-for-byte mutation checks.
func (f *Gateway) Snapshot() map[string][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string][]byte, len(f.objects))
	for key, value := range f.objects {
		out[key] = append([]byte(nil), value...)
	}
	return out
}

func (f *Gateway) ContentType(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.putContentTypes[key]
}
