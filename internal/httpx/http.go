package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

func DecodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(target); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			WriteJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "request body too large"})
			return false
		}
		WriteJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return false
	}
	return true
}

func WriteError(w http.ResponseWriter, status int, err error) {
	WriteJSON(w, status, map[string]any{"error": err.Error()})
}

func WriteJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil && !strings.Contains(err.Error(), "broken pipe") {
		slog.Error("failed to encode response", "error", err)
	}
}

func StatusError(status int, message string) error {
	return httpError{status: status, message: message}
}

type httpError struct {
	status  int
	message string
}

func (e httpError) Error() string { return e.message }

func ErrorStatus(err error, fallback int) int {
	var httpErr httpError
	if errors.As(err, &httpErr) {
		return httpErr.status
	}
	return fallback
}

func QueryInt(r *http.Request, key string, fallback int) int {
	value := r.URL.Query().Get(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

// ReadBodyLimited reads r.Body up to maxBytes+1 bytes and returns the body.
// A nil or empty body returns an empty slice. The caller is responsible for
// restoring r.Body via RestoreBody after a successful read.
func ReadBodyLimited(r *http.Request, maxBytes int64) ([]byte, bool, error) {
	if r.Body == nil {
		return nil, false, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(body)) > maxBytes {
		return nil, true, nil
	}
	return body, false, nil
}

// RestoreBody puts a previously-read body back on the request so downstream
// handlers can read it again.
func RestoreBody(r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
}

// BodyLimitMiddleware rejects request bodies larger than maxBytes with a
// deterministic 413 before any route or auth middleware runs. The body is
// read once, bounded, and restored for the handler. A maxBytes <= 0 disables
// the cap.
func BodyLimitMiddleware(maxBytes int64) func(http.Handler) http.Handler {
	if maxBytes <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body == nil {
				next.ServeHTTP(w, r)
				return
			}
			body, tooLarge, err := ReadBodyLimited(r, maxBytes)
			if err != nil {
				WriteJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
				return
			}
			if tooLarge {
				WriteJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "request body too large"})
				return
			}
			RestoreBody(r, body)
			next.ServeHTTP(w, r)
		})
	}
}
