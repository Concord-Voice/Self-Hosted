package minio_test

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// Regression for #3716 / GO-2026-6609. This exercises the standard-library
// static-file handler used by the console, not a running MinIO binary.
// Run from the derivative's go 1.26.0 module with GOTOOLCHAIN=local and without
// a GODEBUG override: module-off builds select legacy compatibility defaults.
func TestRangeHeaderLimit(t *testing.T) {
	asset := strings.Repeat("0123456789abcdef", 64)
	if len(asset) != 1024 || 201 >= len(asset) {
		t.Fatal("fixture must keep 201 requested bytes below the asset size, avoiding the old aggregate-size fallback")
	}
	handler := http.FileServer(http.FS(fstest.MapFS{
		"asset.txt": {Data: []byte(asset), Mode: 0444},
	}))
	tests := []struct {
		name       string
		rangeValue string
		status     int
		parts      int
		body       string
	}{
		{name: "no range returns the complete asset", status: http.StatusOK, body: asset},
		{name: "one range returns the requested byte", rangeValue: "bytes=0-0", status: http.StatusPartialContent, body: "0"},
		{name: "200 ranges remain supported", rangeValue: "bytes=" + strings.Repeat("0-0,", 199) + "0-0", status: http.StatusPartialContent, parts: 200},
		{name: "201 ranges are ignored with the complete asset", rangeValue: "bytes=" + strings.Repeat("0-0,", 200) + "0-0", status: http.StatusOK, body: asset},
		{name: "invalid range is rejected", rangeValue: "bytes=invalid", status: http.StatusRequestedRangeNotSatisfiable},
		{name: "nonoverlapping range is rejected", rangeValue: "bytes=1024-1025", status: http.StatusRequestedRangeNotSatisfiable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/asset.txt", nil)
			req.Header.Set("Accept-Encoding", "identity")
			if tt.rangeValue != "" {
				req.Header.Set("Range", tt.rangeValue)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.status {
				t.Fatalf("Range handling must %s: got HTTP %d, want %d", tt.name, rec.Code, tt.status)
			}
			if tt.parts > 0 {
				mediaType, params, err := mime.ParseMediaType(rec.Header().Get("Content-Type"))
				if err != nil || mediaType != "multipart/byteranges" || params["boundary"] == "" {
					t.Fatalf("supported multiple ranges must return multipart/byteranges with a boundary: type=%q error=%v", mediaType, err)
				}
				reader := multipart.NewReader(bytes.NewReader(rec.Body.Bytes()), params["boundary"])
				for i := 0; i < tt.parts; i++ {
					part, err := reader.NextPart()
					if err != nil {
						t.Fatalf("supported ranges must contain all %d parts: part %d: %v", tt.parts, i, err)
					}
					body, err := io.ReadAll(part)
					if err != nil || string(body) != "0" || part.Header.Get("Content-Range") != "bytes 0-0/1024" {
						t.Fatalf("range part %d must contain exactly the requested byte and range: body=%q range=%q error=%v", i, body, part.Header.Get("Content-Range"), err)
					}
				}
				if _, err := reader.NextPart(); err != io.EOF {
					t.Fatalf("supported ranges must contain exactly %d parts: trailing result %v", tt.parts, err)
				}
				return
			}
			if tt.status == http.StatusRequestedRangeNotSatisfiable {
				if rec.Body.Len() == 0 {
					t.Fatal("rejected range must return an error response body")
				}
				return
			}
			if rec.Body.String() != tt.body {
				t.Fatalf("Range handling must return the exact expected content: got %d bytes, want %d", rec.Body.Len(), len(tt.body))
			}
			if tt.status == http.StatusPartialContent {
				if got := rec.Header().Get("Content-Range"); got != "bytes 0-0/1024" {
					t.Fatalf("single range must identify the requested byte: Content-Range=%q", got)
				}
			} else if strings.HasPrefix(rec.Header().Get("Content-Type"), "multipart/") || rec.Header().Get("Content-Range") != "" {
				t.Fatal("complete response must not carry multipart or partial-content headers")
			}
		})
	}
}
