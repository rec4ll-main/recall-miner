package httpx

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func okHandler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("handler read: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
}

func TestBodyLimitMiddleware_RejectsOversized(t *testing.T) {
	mw := BodyLimitMiddleware(16)
	handler := mw(okHandler(t))
	req := httptest.NewRequest(http.MethodPost, "/db/1/store", strings.NewReader("this body is longer than sixteen bytes"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", rec.Code)
	}
}

func TestBodyLimitMiddleware_PassesSmallBodyRestored(t *testing.T) {
	mw := BodyLimitMiddleware(1024)
	handler := mw(okHandler(t))
	body := `{"a":1}`
	req := httptest.NewRequest(http.MethodPost, "/db/1/store", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != body {
		t.Fatalf("body = %q, want %q", rec.Body.String(), body)
	}
}

func TestBodyLimitMiddleware_DisabledWhenZero(t *testing.T) {
	mw := BodyLimitMiddleware(0)
	handler := mw(okHandler(t))
	req := httptest.NewRequest(http.MethodPost, "/db/1/store", strings.NewReader("any size is fine when disabled"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 when disabled, got %d", rec.Code)
	}
}
