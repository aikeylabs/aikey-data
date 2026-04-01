package shared

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// JSON writes a JSON response with the given status code.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write json response", "error", err)
	}
}

// ErrorResponse is the standard error envelope.
type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error writes a JSON error response.
func Error(w http.ResponseWriter, status int, code, message string) {
	JSON(w, status, ErrorResponse{Code: code, Message: message})
}
