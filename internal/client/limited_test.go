package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestJSONLimitedRejectsPaddingAndOversizedFields(t *testing.T) {
	for _, body := range []string{`{"proof":"ok","ignored":"` + strings.Repeat("x", 4096) + `"}`, `{"proof":"` + strings.Repeat("x", 4096) + `"}`, `{"proof":"ok"}` + strings.Repeat(" ", 4096), `{"proof":"ok"}{}`} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		var out struct {
			Proof string `json:"proof"`
		}
		e := New(s.Client(), "").RequestJSONLimited(context.Background(), "GET", s.URL, nil, &out, 1024)
		s.Close()
		if e == nil {
			t.Fatal("accepted oversized or trailing response")
		}
	}
}
