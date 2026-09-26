package apiv2

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	chimw "github.com/go-chi/chi/v5/middleware"
)

// TestRawStreamWriterLogsDiscardedBody pins that a raw byte handler's 5xx
// error body, which the adapter replaces with a generic problem, is still
// recorded server-side with its message and the request ID, and never reaches
// the client. A 4xx body stays out of the error log.
func TestRawStreamWriterLogsDiscardedBody(t *testing.T) {
	buf := captureLogs(t)
	r := httptest.NewRequest(http.MethodGet, Prefix+"/stream/session", nil)
	r = r.WithContext(context.WithValue(r.Context(), chimw.RequestIDKey, "raw-stream-request"))

	rec := httptest.NewRecorder()
	w := &streamResponseWriter{ResponseWriter: rec, request: r, problemType: TypeForStatus}
	w.WriteHeader(http.StatusInternalServerError)
	if _, err := w.Write([]byte(`{"error":"internal_error","message":"PRIVATE_DETAIL"}`)); err != nil {
		t.Fatal(err)
	}

	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "PRIVATE_DETAIL") {
		t.Fatalf("client response: %d %s", rec.Code, rec.Body.String())
	}
	var p problemDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("problem body: %v", err)
	}
	if p.Type != TypeInternalError.URI() || p.Detail == "" {
		t.Fatalf("problem envelope = %+v", p)
	}

	line := buf.String()
	for _, want := range []string{`"component":"apiv2"`, `"status":500`, `"request_id":"raw-stream-request"`, "PRIVATE_DETAIL"} {
		if !strings.Contains(line, want) {
			t.Errorf("log line lacks %s: %s", want, line)
		}
	}

	// A 4xx discard is client-facing and stays out of the error log.
	buf.Reset()
	rec = httptest.NewRecorder()
	w = &streamResponseWriter{ResponseWriter: rec, request: r, problemType: TypeForStatus}
	w.WriteHeader(http.StatusNotFound)
	if _, err := w.Write([]byte(`{"error":"not_found","message":"gone"}`)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "gone") {
		t.Fatalf("4xx body logged: %s", buf.String())
	}
}
