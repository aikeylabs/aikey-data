package shared

import (
	"net/http"
	"strings"
)

// ServiceTokenAuth validates the Bearer token against the expected service token.
func ServiceTokenAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next // no auth configured
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || strings.TrimPrefix(auth, "Bearer ") != token {
			Error(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid or missing service token")
			return
		}
		next.ServeHTTP(w, r)
	})
}
