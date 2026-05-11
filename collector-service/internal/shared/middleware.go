package shared

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// ingestSubjectKey is the context key used by IngestAuth to attach the
// authenticated subject to the request context. Downstream handlers
// (HandleBatch) call IngestSubject to retrieve it.
//
// Why a private package-level type: the standard pattern for context
// keys — prevents external packages from accidentally colliding by
// using a string key with the same value.
type ingestSubjectKey struct{}

// IngestSubject describes who authenticated the current ingest request.
// Attached to request context by IngestAuth; read by ingest handlers
// when they need to force-override client-claimed fields (e.g. the
// 2026-05-11 B-phase contract that user-JWT requests have their
// per-event account_id overwritten with JWT.AccountID).
//
// Source semantics:
//   - "jwt"           — request authenticated via a user JWT signed by
//                       control-service. AccountID is authoritative.
//   - "service_token" — legacy S2S path; client may set any account_id
//                       in event payloads (trust mode, escape hatch).
type IngestSubject struct {
	Source    string // "jwt" | "service_token"
	AccountID string // populated only when Source == "jwt"
}

// WithIngestSubject puts an IngestSubject into ctx. Test helpers may
// call this directly; production wiring goes through IngestAuth.
func WithIngestSubject(ctx context.Context, s IngestSubject) context.Context {
	return context.WithValue(ctx, ingestSubjectKey{}, s)
}

// GetIngestSubject pulls the subject IngestAuth attached. Second return
// is false when no subject is on the context (e.g. the handler was
// reached through a non-IngestAuth path; treat as legacy / unauth'd).
func GetIngestSubject(ctx context.Context) (IngestSubject, bool) {
	s, ok := ctx.Value(ingestSubjectKey{}).(IngestSubject)
	return s, ok
}

// minimalClaims mirrors aikey-control-service's shared.Claims subset
// the collector cares about. We DON'T import the control-service
// package because collector-service and control-service are separate
// Go modules — coupling them at the import level just to share a JWT
// shape would force every collector deployment to also vendor the
// control-service tree. The two services share **the signing secret**,
// not the code: as long as control-service signs with HS256 over a
// payload that includes `account_id`, we can verify it here.
type minimalClaims struct {
	AccountID string `json:"account_id"`
	jwt.RegisteredClaims
}

// IngestAuth replaces ServiceTokenAuth (2026-05-11 B-phase). Two
// authentication paths, JWT-first:
//
//  1. JWT path. Bearer is parsed against jwtSecret. On success the
//     authenticated account_id is attached to request context via
//     IngestSubject{Source: "jwt"}. Downstream handlers MUST overwrite
//     client-claimed account_id with this value (see ingest.go).
//
//  2. service_token fallback. If JWT parse fails AND fallbackToken
//     is non-empty, compare bearer constant-time against it. On match,
//     attach IngestSubject{Source: "service_token"} (no AccountID —
//     handlers MUST trust client claim under this path; it's the
//     admin escape hatch).
//
// Either path missing / failing → 401. fallbackToken == "" disables
// the service_token branch entirely (default for new deployments per
// the 20260511-user-jwt-collector-ingest.md design).
//
// Why constant-time compare for the fallback: the historical
// ServiceTokenAuth used `strings.TrimPrefix(...) != token` which is
// timing-vulnerable in principle. Tightening on the way through.
func IngestAuth(jwtSecret []byte, fallbackToken string, next http.Handler) http.Handler {
	if len(jwtSecret) == 0 && fallbackToken == "" {
		// No auth configured at all — preserve historical
		// ServiceTokenAuth behaviour (open ingest, useful for local
		// dev / CI where no secret is provisioned).
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		bearer, ok := strings.CutPrefix(auth, "Bearer ")
		if !ok {
			Error(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid or missing service token")
			return
		}
		bearer = strings.TrimSpace(bearer)

		// JWT-first path.
		if len(jwtSecret) > 0 {
			if claims, err := parseAndVerifyJWT(bearer, jwtSecret); err == nil {
				ctx := WithIngestSubject(r.Context(), IngestSubject{
					Source:    "jwt",
					AccountID: claims.AccountID,
				})
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			// Fall through to service_token path — common when a CLI
			// hasn't logged in yet and is using the legacy token, or
			// when a JWT is malformed. We don't surface the JWT parse
			// error to the client to avoid leaking signing-method or
			// expiry info to unauthenticated callers.
		}

		// service_token fallback (escape hatch).
		if fallbackToken != "" &&
			subtle.ConstantTimeCompare([]byte(bearer), []byte(fallbackToken)) == 1 {
			ctx := WithIngestSubject(r.Context(), IngestSubject{Source: "service_token"})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		Error(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid or missing service token")
	})
}

// parseAndVerifyJWT extracts minimalClaims from a HS256-signed token.
// Returns an error on any of: malformed token, wrong signing method,
// signature mismatch, expired/not-yet-valid, missing account_id.
func parseAndVerifyJWT(tokenStr string, secret []byte) (*minimalClaims, error) {
	tok, err := jwt.ParseWithClaims(tokenStr, &minimalClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return secret, nil
	})
	if err != nil {
		return nil, fmt.Errorf("parse token: %w", err)
	}
	claims, ok := tok.Claims.(*minimalClaims)
	if !ok || !tok.Valid {
		return nil, fmt.Errorf("invalid token claims")
	}
	if claims.AccountID == "" {
		// A signed-but-account_id-less JWT is structurally legal but
		// useless for ingest's "force-overwrite client-claimed
		// account_id" contract. Reject loud here so the caller doesn't
		// silently fall through to "trust client claim" mode.
		return nil, fmt.Errorf("token missing account_id claim")
	}
	return claims, nil
}

// ServiceTokenAuth is kept as a back-compat alias for IngestAuth with
// no JWT secret configured — equivalent to the old "service_token
// only" middleware. Lets callers that haven't migrated to per-route
// credentials yet keep working unchanged.
//
// Deprecated: prefer IngestAuth directly so the JWT path is wired in.
func ServiceTokenAuth(token string, next http.Handler) http.Handler {
	return IngestAuth(nil, token, next)
}
