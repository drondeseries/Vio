package apiv2

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestRequestLogCarries500Cause pins that a 500 request log names the
// underlying application error under the same request ID, for both shapes an
// operation can fail with:
//
//   - a plain (non-StatusError) error, which Huma converts to the fixed
//     internal_error envelope and would otherwise discard;
//   - a service error masked by serviceProblem, whose cause is carried on the
//     Problem for the log and never the response body.
func TestRequestLogCarries500Cause(t *testing.T) {
	buf := captureLogs(t)
	h := NewHandler(Dependencies{testRegister: func(reg *Registry) {
		Register(reg, Operation{
			Operation: humaOp(http.MethodGet, Prefix+"/probe/fail/internal", "probeFailInternal", "probe", "masked service error"),
			Class:     ClassPublic,
		}, func(context.Context, *struct{}) (*probeOutput, error) {
			return nil, serviceProblem(errors.New("boom masked cause"))
		})
		Register(reg, Operation{
			Operation: humaOp(http.MethodGet, Prefix+"/probe/fail/plain", "probeFailPlain", "probe", "plain error"),
			Class:     ClassPublic,
		}, func(context.Context, *struct{}) (*probeOutput, error) {
			return nil, errors.New("boom plain cause")
		})
	}})

	cases := []struct {
		path string
		want string
	}{
		{"/api/v2/probe/fail/internal", "boom masked cause"},
		{"/api/v2/probe/fail/plain", "boom plain cause"},
	}
	for _, tc := range cases {
		buf.Reset()
		rec := do(t, h, http.MethodGet, tc.path, "", nil)
		requireProblem(t, rec, TypeInternalError)
		line := buf.String()
		if !strings.Contains(line, `"status":500`) {
			t.Errorf("%s: log line does not report the 500: %s", tc.path, line)
		}
		if !strings.Contains(line, tc.want) {
			t.Errorf("%s: log line does not name the cause %q: %s", tc.path, tc.want, line)
		}
		if !strings.Contains(line, `"request_id":"`+requestIDHeader(rec)+`"`) {
			t.Errorf("%s: log line does not carry the request ID: %s", tc.path, line)
		}
		if !strings.Contains(line, `"error_code":"internal_error"`) {
			t.Errorf("%s: log line does not carry the problem code: %s", tc.path, line)
		}
	}
}
