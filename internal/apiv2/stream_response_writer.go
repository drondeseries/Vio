package apiv2

import (
	"log/slog"
	"net/http"
)

// streamResponseWriter adapts legacy byte handlers to the v2 problem format.
// It owns commitment tracking so every byte endpoint handles pre-body errors,
// late failures, and flushing the same way.
type streamResponseWriter struct {
	http.ResponseWriter
	request       *http.Request
	problemType   func(int) ProblemType
	redactHeaders []string
	status        int
	rejected      bool
	// discardLogged guards the single log line for the legacy error body the
	// adapter replaces with a problem envelope, so a handler that writes it in
	// pieces does not log it repeatedly.
	discardLogged bool
}

func (w *streamResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *streamResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		if status >= 400 && !w.rejected {
			panic(http.ErrAbortHandler)
		}
		return
	}
	if status < 200 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.status = status
	if status < 400 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.rejected = true
	for _, header := range w.redactHeaders {
		w.Header().Del(header)
	}
	kind := w.problemType(status)
	writeProblem(w.ResponseWriter, w.request, NewProblem(kind, kind.Title))
}

func (w *streamResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.rejected {
		w.logDiscardedBody(data)
		return len(data), nil
	}
	return w.ResponseWriter.Write(data)
}

// logDiscardedBody records the legacy v1 error body the adapter refuses to
// forward in favor of a problem envelope. A raw 5xx otherwise reaches the
// request log as only the generic problem code: the machine-readable code and
// human-readable message the v1 handler wrote are lost. It logs once per
// response, under the request ID, and the body never reaches the client.
func (w *streamResponseWriter) logDiscardedBody(data []byte) {
	if w.discardLogged || w.status < http.StatusInternalServerError {
		return
	}
	w.discardLogged = true
	slog.ErrorContext(w.request.Context(), "apiv2 raw stream error body discarded",
		"component", "apiv2",
		"request_id", requestIDFrom(w.request.Context()),
		"status", w.status,
		"discarded", string(data))
}

func (w *streamResponseWriter) FlushError() error {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}
