// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package s3test

import (
	"encoding/xml"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Server is an in-memory S3 server, path-style, with the calls the
// backend makes: objects with conditional PUT and ranged GET, listing
// with continuation, and multipart uploads. Modes and hooks inject the
// behaviours of other S3-compatible services and of failures. It serves
// any bucket name and needs no credentials.
type Server struct {
	mu       sync.Mutex
	objects  map[string]object
	uploads  map[string]map[int][]byte
	nextID   int
	requests map[string]int

	// PageSize is the most keys one listing page returns.
	PageSize int
	// CCMode is "" (honour If-None-Match), "501" (refuse it) or "ignore".
	CCMode string
	// Fault, if set, may answer a request instead of the server: it
	// returns true when it wrote a response. Set it before requests start.
	Fault func(w http.ResponseWriter, r *http.Request) bool
}

type object struct {
	data []byte
	etag string
	mod  time.Time
}

// New starts a server, stopped when the test ends, and returns it with
// its URL.
func New(t testing.TB) (*Server, string) {
	t.Helper()
	f := &Server{objects: map[string]object{}, uploads: map[string]map[int][]byte{}, PageSize: 1000, requests: map[string]int{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

// Count returns how many requests of a kind the server saw: an HTTP
// method, or create-multipart, upload-part, complete-multipart,
// abort-multipart or list.
func (f *Server) Count(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[kind]
}

// Uploads returns how many multipart uploads are in progress.
func (f *Server) Uploads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.uploads)
}

// Error writes an S3 error response.
func Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
}

// etagOf is an opaque ETag for content; the backend only compares them.
func etagOf(b []byte) string {
	h := fnv.New64a()
	_, _ = h.Write(b)
	return fmt.Sprintf(`"%016x-%d"`, h.Sum64(), len(b))
}

func (f *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	_, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	kind := r.Method
	switch {
	case q.Has("uploads"):
		kind = "create-multipart"
	case q.Has("partNumber"):
		kind = "upload-part"
	case q.Has("uploadId") && r.Method == http.MethodPost:
		kind = "complete-multipart"
	case q.Has("uploadId"):
		kind = "abort-multipart"
	case q.Get("list-type") == "2":
		kind = "list"
	}
	f.mu.Lock()
	f.requests[kind]++
	fault := f.Fault
	f.mu.Unlock()
	if fault != nil && fault(w, r) {
		return
	}
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch kind {
	case "list":
		f.list(w, q.Get("prefix"), q.Get("continuation-token"))
	case "create-multipart":
		f.nextID++
		id := strconv.Itoa(f.nextID)
		f.uploads[id] = map[int][]byte{}
		_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><InitiateMultipartUploadResult><Bucket>b</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, key, id) // #nosec G705 -- a test server echoing test keys
	case "upload-part":
		parts, ok := f.uploads[q.Get("uploadId")]
		if !ok {
			Error(w, 404, "NoSuchUpload")
			return
		}
		n, _ := strconv.Atoi(q.Get("partNumber"))
		parts[n] = body
		w.Header().Set("ETag", etagOf(body))
	case "complete-multipart":
		parts, ok := f.uploads[q.Get("uploadId")]
		if !ok {
			Error(w, 404, "NoSuchUpload")
			return
		}
		if !f.conditionOK(w, r, key) {
			return
		}
		var req struct {
			Parts []struct {
				PartNumber int
			} `xml:"Part"`
		}
		if err := xml.Unmarshal(body, &req); err != nil {
			Error(w, 400, "MalformedXML")
			return
		}
		var data []byte
		for _, p := range req.Parts {
			data = append(data, parts[p.PartNumber]...)
		}
		delete(f.uploads, q.Get("uploadId"))
		f.objects[key] = object{data: data, etag: etagOf(data), mod: time.Now()}
		_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><CompleteMultipartUploadResult><Key>%s</Key><ETag>%s</ETag></CompleteMultipartUploadResult>`, key, etagOf(data)) // #nosec G705 -- a test server echoing test keys
	case "abort-multipart":
		delete(f.uploads, q.Get("uploadId"))
		w.WriteHeader(204)
	case http.MethodPut:
		if !f.conditionOK(w, r, key) {
			return
		}
		f.objects[key] = object{data: body, etag: etagOf(body), mod: time.Now()}
		w.Header().Set("ETag", etagOf(body))
	case http.MethodHead:
		o, ok := f.objects[key]
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(o.data)))
		w.Header().Set("ETag", o.etag)
		w.Header().Set("Last-Modified", o.mod.UTC().Format(http.TimeFormat))
	case http.MethodGet:
		f.get(w, r, key)
	case http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(204)
	default:
		Error(w, 405, "MethodNotAllowed")
	}
}

// conditionOK applies If-None-Match as the mode says; false means it
// answered.
func (f *Server) conditionOK(w http.ResponseWriter, r *http.Request, key string) bool {
	if r.Header.Get("If-None-Match") != "*" {
		return true
	}
	switch f.CCMode {
	case "501":
		Error(w, 501, "NotImplemented")
		return false
	case "ignore":
		return true
	}
	if _, exists := f.objects[key]; exists {
		Error(w, 412, "PreconditionFailed")
		return false
	}
	return true
}

func (f *Server) get(w http.ResponseWriter, r *http.Request, key string) {
	o, ok := f.objects[key]
	if !ok {
		Error(w, 404, "NoSuchKey")
		return
	}
	if m := r.Header.Get("If-Match"); m != "" && m != o.etag {
		Error(w, 412, "PreconditionFailed")
		return
	}
	size := int64(len(o.data))
	start, end := int64(0), size-1
	status := 200
	if rg := r.Header.Get("Range"); rg != "" {
		a, b, _ := strings.Cut(strings.TrimPrefix(rg, "bytes="), "-")
		start, _ = strconv.ParseInt(a, 10, 64)
		if b != "" {
			end, _ = strconv.ParseInt(b, 10, 64)
		}
		if start >= size {
			Error(w, 416, "InvalidRange")
			return
		}
		end = min(end, size-1)
		status = 206
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	}
	w.Header().Set("ETag", o.etag)
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(status)
	_, _ = w.Write(o.data[start : end+1])
}

func (f *Server) list(w http.ResponseWriter, prefix, token string) {
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) && k > token {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	truncated := len(keys) > f.PageSize
	if truncated {
		keys = keys[:f.PageSize]
	}
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult>`)
	for _, k := range keys {
		o := f.objects[k]
		fmt.Fprintf(&sb, `<Contents><Key>%s</Key><Size>%d</Size><LastModified>%s</LastModified><ETag>%s</ETag></Contents>`,
			k, len(o.data), o.mod.UTC().Format(time.RFC3339), o.etag)
	}
	fmt.Fprintf(&sb, `<KeyCount>%d</KeyCount><IsTruncated>%v</IsTruncated>`, len(keys), truncated)
	if truncated {
		fmt.Fprintf(&sb, `<NextContinuationToken>%s</NextContinuationToken>`, keys[len(keys)-1])
	}
	sb.WriteString(`</ListBucketResult>`)
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, sb.String())
}
