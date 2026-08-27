// Package httpx holds transport-level helpers: JSON encoding, middleware and
// the server lifecycle. It knows about HTTP; it knows nothing about providers.
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Envelope wraps every successful response body.
type Envelope struct {
	Data any `json:"data"`
}

// ErrorBody is the single error shape all endpoints return.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// JSON writes v as JSON with the given status.
func JSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		slog.ErrorContext(r.Context(), "encode response", "error", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"failed to encode response"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

// Data writes a successful envelope.
func Data(w http.ResponseWriter, r *http.Request, status int, v any) {
	JSON(w, r, status, Envelope{Data: v})
}

// Error writes the canonical error body.
func Error(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	JSON(w, r, status, ErrorBody{Error: ErrorDetail{
		Code:      code,
		Message:   message,
		RequestID: RequestIDFrom(r.Context()),
	}})
}
