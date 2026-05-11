package shared

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// echoSubjectHandler reports back what the middleware attached to the
// request context. Tests use it as the protected next-handler so they
// can assert "the JWT subject was wired through" without depending on
// the production ingest handler.
func echoSubjectHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s, ok := GetIngestSubject(r.Context()); ok {
			_, _ = w.Write([]byte(s.Source + ":" + s.AccountID))
			return
		}
		_, _ = w.Write([]byte("no-subject"))
	}
}

// signTestToken mints a HS256 JWT shaped like control-service's user
// access token. Test-local — we don't import control-service's
// TokenService because the two services live in independent Go modules
// (the only thing they share by design is the secret bytes, not the
// code).
func signTestToken(t *testing.T, secret []byte, accountID string, opts ...func(*minimalClaims)) string {
	t.Helper()
	claims := &minimalClaims{
		AccountID: accountID,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
			Issuer:    "test",
		},
	}
	for _, opt := range opts {
		opt(claims)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString(secret)
	if err != nil {
		t.Fatalf("sign test token: %v", err)
	}
	return signed
}

// ── 1. JWT-first path ──────────────────────────────────────────────────

// 2026-05-11 B-phase contract: a valid bearer JWT puts the JWT's
// account_id on context as Source="jwt". Downstream ingest handler
// then force-overwrites every event's AccountID. This test pins the
// upstream half of that contract.
func TestIngestAuth_ValidJWTPopulatesIngestSubject(t *testing.T) {
	secret := []byte("supersecret-key-32-bytes-or-more...")
	srv := httptest.NewServer(IngestAuth(secret, "", echoSubjectHandler()))
	defer srv.Close()

	tok := signTestToken(t, secret, "acct-abc")
	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status=%d, want 200 (auth should pass)", resp.StatusCode)
	}
	body := make([]byte, 256)
	n, _ := resp.Body.Read(body)
	if got := string(body[:n]); got != "jwt:acct-abc" {
		t.Fatalf("subject leak: got %q, want jwt:acct-abc", got)
	}
}

// Tampering with the JWT body changes the signature → must reject.
// Crucial: an attacker can't forge account_id by editing the JWT after
// the issuer signed it.
func TestIngestAuth_TamperedJWTRejected(t *testing.T) {
	secret := []byte("supersecret-key-32-bytes-or-more...")
	srv := httptest.NewServer(IngestAuth(secret, "", echoSubjectHandler()))
	defer srv.Close()

	tok := signTestToken(t, secret, "acct-abc")
	// Flip a byte in the payload section (middle of the dotted form).
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("unexpected JWT shape: %d parts", len(parts))
	}
	parts[1] = parts[1][:len(parts[1])-1] + "X"
	tampered := strings.Join(parts, ".")

	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Authorization", "Bearer "+tampered)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("tampered JWT must 401; got %d", resp.StatusCode)
	}
}

// Different signing secret → reject. Catches "deployer set the wrong
// JWT_SECRET on the collector but JWT path still let stuff through"
// regressions.
func TestIngestAuth_JWTSignedWithWrongSecretRejected(t *testing.T) {
	good := []byte("the-real-secret-XXXXXXXXXXXXXXXXXXXX")
	bad := []byte("attacker-controlled-XXXXXXXXXXXXXXXX")
	srv := httptest.NewServer(IngestAuth(good, "", echoSubjectHandler()))
	defer srv.Close()

	tok := signTestToken(t, bad, "acct-abc")
	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong-secret JWT must 401; got %d", resp.StatusCode)
	}
}

// Expired JWT must reject — covers the post-(access_token TTL) state
// when the proxy fails to refresh. We rely on this rejection to land
// the upload in dead-letter.jsonl (terminal failure) so operators
// notice rather than silently dropping events.
func TestIngestAuth_ExpiredJWTRejected(t *testing.T) {
	secret := []byte("supersecret-key-32-bytes-or-more...")
	srv := httptest.NewServer(IngestAuth(secret, "", echoSubjectHandler()))
	defer srv.Close()

	tok := signTestToken(t, secret, "acct-abc", func(c *minimalClaims) {
		c.IssuedAt = jwt.NewNumericDate(time.Now().Add(-2 * time.Hour))
		c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-1 * time.Hour))
	})
	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired JWT must 401; got %d", resp.StatusCode)
	}
}

// JWT missing account_id claim must reject. Force-overwrite contract
// downstream is "use jwt.account_id" — accepting a tokenless of that
// claim would break the invariant.
func TestIngestAuth_JWTWithoutAccountIDRejected(t *testing.T) {
	secret := []byte("supersecret-key-32-bytes-or-more...")
	srv := httptest.NewServer(IngestAuth(secret, "", echoSubjectHandler()))
	defer srv.Close()

	tok := signTestToken(t, secret, "" /* empty account_id */)
	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing account_id JWT must 401; got %d", resp.StatusCode)
	}
}

// ── 2. service_token fallback path ────────────────────────────────────

// service_token is the admin escape hatch. Matching bearer → request
// passes through with Source="service_token". Downstream handlers MUST
// trust client-claimed account_id (no overwrite) under this source.
func TestIngestAuth_ServiceTokenMatchPopulatesSubject(t *testing.T) {
	srv := httptest.NewServer(IngestAuth(nil, "admin-svc-token", echoSubjectHandler()))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Authorization", "Bearer admin-svc-token")
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	body := make([]byte, 256)
	n, _ := resp.Body.Read(body)
	if got := string(body[:n]); got != "service_token:" {
		t.Fatalf("subject leak: got %q, want service_token:", got)
	}
}

func TestIngestAuth_ServiceTokenMismatchRejects(t *testing.T) {
	srv := httptest.NewServer(IngestAuth(nil, "admin-svc-token", echoSubjectHandler()))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong service_token must 401; got %d", resp.StatusCode)
	}
}

// ── 3. JWT + service_token coexistence ────────────────────────────────

// Both auth methods are configured. A valid JWT wins (JWT-first); the
// service_token still works as a fallback path. Models the production
// deployment where most clients use JWTs but admin can still bypass
// via service_token.
func TestIngestAuth_JWTPreferredOverServiceToken(t *testing.T) {
	secret := []byte("supersecret-key-32-bytes-or-more...")
	srv := httptest.NewServer(IngestAuth(secret, "admin-svc-token", echoSubjectHandler()))
	defer srv.Close()

	// Use a valid JWT — middleware should attach jwt source, NOT
	// fallthrough to service_token check.
	tok := signTestToken(t, secret, "acct-via-jwt")
	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	body := make([]byte, 256)
	n, _ := resp.Body.Read(body)
	if got := string(body[:n]); got != "jwt:acct-via-jwt" {
		t.Fatalf("got %q, want jwt:acct-via-jwt (JWT path should win)", got)
	}
}

// Invalid JWT but matching service_token → fall through to escape
// hatch. This is the legacy-CLI case: it doesn't know how to mint a
// JWT but is sending a recognised service_token.
func TestIngestAuth_FallsThroughToServiceTokenWhenJWTInvalid(t *testing.T) {
	secret := []byte("supersecret-key-32-bytes-or-more...")
	srv := httptest.NewServer(IngestAuth(secret, "admin-svc-token", echoSubjectHandler()))
	defer srv.Close()

	// Send the service_token in the Bearer slot (not a JWT). The JWT
	// parse will fail, then the service_token check should match.
	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Authorization", "Bearer admin-svc-token")
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	body := make([]byte, 256)
	n, _ := resp.Body.Read(body)
	if got := string(body[:n]); got != "service_token:" {
		t.Fatalf("got %q, want service_token: (fallback should engage)", got)
	}
}

// Both invalid → 401, no subject attached.
func TestIngestAuth_BothInvalidReturns401(t *testing.T) {
	secret := []byte("supersecret-key-32-bytes-or-more...")
	srv := httptest.NewServer(IngestAuth(secret, "admin-svc-token", echoSubjectHandler()))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Authorization", "Bearer garbage")
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", resp.StatusCode)
	}
}

// ── 4. Missing / malformed Authorization header ───────────────────────

func TestIngestAuth_MissingAuthHeaderRejected(t *testing.T) {
	secret := []byte("supersecret-key-32-bytes-or-more...")
	srv := httptest.NewServer(IngestAuth(secret, "admin-svc-token", echoSubjectHandler()))
	defer srv.Close()

	resp, _ := http.Get(srv.URL)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing Authorization must 401; got %d", resp.StatusCode)
	}
}

func TestIngestAuth_NonBearerSchemeRejected(t *testing.T) {
	secret := []byte("supersecret-key-32-bytes-or-more...")
	srv := httptest.NewServer(IngestAuth(secret, "admin-svc-token", echoSubjectHandler()))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("non-Bearer scheme must 401; got %d", resp.StatusCode)
	}
}

// ── 5. Open path (no auth configured) ────────────────────────────────

// Dev / CI deployments may run with no secrets at all. Middleware must
// pass requests through unchanged so the open ingest endpoint keeps
// working — same behaviour as the legacy ServiceTokenAuth(token="").
func TestIngestAuth_NoAuthConfiguredPassesThrough(t *testing.T) {
	srv := httptest.NewServer(IngestAuth(nil, "", echoSubjectHandler()))
	defer srv.Close()

	resp, _ := http.Get(srv.URL)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("no-auth dev path must pass; got %d", resp.StatusCode)
	}
	body := make([]byte, 256)
	n, _ := resp.Body.Read(body)
	if got := string(body[:n]); got != "no-subject" {
		t.Fatalf("no-auth path should not attach subject; got %q", got)
	}
}

// ── 6. Back-compat alias ──────────────────────────────────────────────

func TestServiceTokenAuth_BackCompatAliasStillWorks(t *testing.T) {
	srv := httptest.NewServer(ServiceTokenAuth("legacy-token", echoSubjectHandler()))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Authorization", "Bearer legacy-token")
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("legacy alias must pass matching token; got %d", resp.StatusCode)
	}
}

// ── 7. Context plumbing helpers ───────────────────────────────────────

func TestWithIngestSubject_RoundTrips(t *testing.T) {
	ctx := WithIngestSubject(context.Background(), IngestSubject{
		Source:    "jwt",
		AccountID: "acct-z",
	})
	s, ok := GetIngestSubject(ctx)
	if !ok {
		t.Fatal("subject not retrievable")
	}
	if s.Source != "jwt" || s.AccountID != "acct-z" {
		t.Fatalf("got %+v", s)
	}
}

func TestGetIngestSubject_AbsentReturnsFalse(t *testing.T) {
	_, ok := GetIngestSubject(context.Background())
	if ok {
		t.Fatal("empty context should return ok=false")
	}
}
