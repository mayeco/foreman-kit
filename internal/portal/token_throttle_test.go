package portal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func sendPortalReq(t *testing.T, srv http.Handler, path, remoteAddr, token string) (int, map[string]any, string, http.Header) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4567"+path, nil)
	req.Host = "127.0.0.1:4567"
	req.RemoteAddr = remoteAddr
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	raw := rec.Body.String()
	var parsed map[string]any
	_ = json.Unmarshal([]byte(raw), &parsed)
	return rec.Code, parsed, raw, rec.Header()
}

func TestPortalTokenThrottle(t *testing.T) {
	origOut := log.Writer()
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(origOut)

	srv, _, _ := setupTestPortal(t)
	const validToken = "super-secret-portal-token-abc123"
	t.Setenv("FOREMANKIT_PORTAL_TOKEN", validToken)

	now := time.Date(2026, 11, 13, 14, 0, 0, 0, time.UTC)
	srv.tokens.now = func() time.Time { return now }

	// Loopback request to loopback-bound portal needs no token before any remote failures.
	if code, _, raw, _ := sendPortalReq(t, srv, "/api/dashboard", "127.0.0.1:1234", ""); code != http.StatusOK {
		t.Fatalf("loopback tokenless GET /api/dashboard returned %d: %s", code, raw)
	}

	// Remote request with the correct token succeeds before lockout.
	if code, _, raw, _ := sendPortalReq(t, srv, "/api/dashboard", "192.168.4.7:5555", validToken); code != http.StatusOK {
		t.Fatalf("remote GET /api/dashboard with valid token returned %d: %s", code, raw)
	}

	// Tokenless remote requests answer 403 forbidden and are not counted toward lockout.
	for i := 0; i < 15; i++ {
		code, data, raw, _ := sendPortalReq(t, srv, "/api/dashboard", "192.168.4.7:5555", "")
		if code != http.StatusForbidden {
			t.Fatalf("tokenless remote attempt %d got %d, want 403: %s", i+1, code, raw)
		}
		if errCode, _ := errorOf(data); errCode != "forbidden" {
			t.Fatalf("tokenless remote attempt %d error.code = %q, want forbidden: %s", i+1, errCode, raw)
		}
	}

	// 10 wrong tokens from 192.168.4.7:5555 within one minute answer 403 forbidden.
	for i := 1; i <= 10; i++ {
		code, data, raw, _ := sendPortalReq(t, srv, "/api/dashboard", "192.168.4.7:5555", fmt.Sprintf("wrong-token-%d", i))
		if code != http.StatusForbidden {
			t.Fatalf("wrong token attempt %d got %d, want 403: %s", i, code, raw)
		}
		if errCode, _ := errorOf(data); errCode != "forbidden" {
			t.Fatalf("wrong token attempt %d error.code = %q, want forbidden: %s", i, errCode, raw)
		}
		now = now.Add(2 * time.Second)
	}

	// The 11th wrong token answers 429 with Retry-After in 1..300 and error.code rate_limited.
	code, data, raw, hdr := sendPortalReq(t, srv, "/api/dashboard", "192.168.4.7:5555", "wrong-token-11")
	if code != http.StatusTooManyRequests {
		t.Fatalf("11th wrong token got %d, want 429: %s", code, raw)
	}
	if errCode, _ := errorOf(data); errCode != "rate_limited" {
		t.Fatalf("11th wrong token error.code = %q, want rate_limited: %s", errCode, raw)
	}
	retrySecs, err := strconv.Atoi(hdr.Get("Retry-After"))
	if err != nil || retrySecs < 1 || retrySecs > 300 {
		t.Fatalf("11th wrong token Retry-After = %q (err=%v), want 1..300", hdr.Get("Retry-After"), err)
	}

	// The correct token from the locked-out address is also refused 429 with Retry-After before the window ends.
	code, data, raw, hdr = sendPortalReq(t, srv, "/api/dashboard", "192.168.4.7:5555", validToken)
	if code != http.StatusTooManyRequests {
		t.Fatalf("locked-out address with valid token got %d, want 429: %s", code, raw)
	}
	if errCode, _ := errorOf(data); errCode != "rate_limited" {
		t.Fatalf("locked-out address with valid token error.code = %q, want rate_limited: %s", errCode, raw)
	}
	if retrySecs, err := strconv.Atoi(hdr.Get("Retry-After")); err != nil || retrySecs < 1 || retrySecs > 300 {
		t.Fatalf("locked-out address Retry-After = %q (err=%v), want 1..300", hdr.Get("Retry-After"), err)
	}

	// A page GET / from the locked-out address answers 429 with Retry-After, even with the correct token.
	code, _, raw, hdr = sendPortalReq(t, srv, "/", "192.168.4.7:5555", validToken)
	if code != http.StatusTooManyRequests {
		t.Fatalf("page GET / from locked-out address got %d, want 429: %s", code, raw)
	}
	if retrySecs, err := strconv.Atoi(hdr.Get("Retry-After")); err != nil || retrySecs < 1 || retrySecs > 300 {
		t.Fatalf("page GET / Retry-After = %q (err=%v), want 1..300", hdr.Get("Retry-After"), err)
	}

	// Another remote address (192.168.4.8) with the correct token is answered 200 while 192.168.4.7 is locked out.
	if code, _, raw, _ := sendPortalReq(t, srv, "/api/dashboard", "192.168.4.8:4444", validToken); code != http.StatusOK {
		t.Fatalf("GET /api/dashboard from 192.168.4.8 with valid token got %d, want 200: %s", code, raw)
	}

	// A tokenless loopback request from 127.0.0.1:1234 is still answered 200 while 192.168.4.7 is locked out.
	if code, _, raw, _ := sendPortalReq(t, srv, "/api/dashboard", "127.0.0.1:1234", ""); code != http.StatusOK {
		t.Fatalf("loopback tokenless GET /api/dashboard during remote lockout got %d, want 200: %s", code, raw)
	}

	// Once the injected clock passes the 5-minute window, 192.168.4.7 with the correct token is answered 200.
	now = time.Date(2026, 11, 13, 14, 5, 1, 0, time.UTC)
	if code, _, raw, _ := sendPortalReq(t, srv, "/api/dashboard", "192.168.4.7:5555", validToken); code != http.StatusOK {
		t.Fatalf("GET /api/dashboard from 192.168.4.7 after window expiry got %d, want 200: %s", code, raw)
	}
}

func TestPortalTokenThrottleLog(t *testing.T) {
	srv, _, _ := setupTestPortal(t)
	const validToken = "expected-portal-token-never-logged-98765"
	t.Setenv("FOREMANKIT_PORTAL_TOKEN", validToken)

	now := time.Date(2026, 11, 13, 15, 0, 0, 0, time.UTC)
	srv.tokens.now = func() time.Time { return now }

	origOut := log.Writer()
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(origOut)

	var sentTokens []string
	for i := 1; i <= 50; i++ {
		tok := fmt.Sprintf("candidate-secret-guess-%02d", i)
		sentTokens = append(sentTokens, tok)
		code, _, raw, _ := sendPortalReq(t, srv, "/api/dashboard", "192.168.4.7:5555", tok)
		if i <= 10 && code != http.StatusForbidden {
			t.Fatalf("attempt %d: got %d, want 403: %s", i, code, raw)
		}
		if i > 10 && code != http.StatusTooManyRequests {
			t.Fatalf("attempt %d: got %d, want 429: %s", i, code, raw)
		}
	}

	logOutput := logBuf.String()
	lines := strings.Split(strings.TrimSpace(logOutput), "\n")
	addrLines := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.Contains(line, "192.168.4.7") {
			addrLines++
		}
		if strings.Contains(line, validToken) {
			t.Fatalf("log line leaked expected token %q: %s", validToken, line)
		}
		for _, tok := range sentTokens {
			if strings.Contains(line, tok) {
				t.Fatalf("log line leaked provided token %q: %s", tok, line)
			}
		}
	}
	if addrLines == 0 || addrLines > 2 {
		t.Fatalf("expected 1..2 log lines naming 192.168.4.7 across 50 wrong tokens, got %d:\n%s", addrLines, logOutput)
	}
}
