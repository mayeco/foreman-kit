package portal

import (
	"net/http"
	"testing"
)

// Rendered pages refuse to be framed (clickjacking): their actions are same-origin requests that pass
// the API's CSRF checks from inside a frame.
func TestPortalPagesRefuseToBeFramed(t *testing.T) {
	srv, _, _ := setupTestPortal(t)
	for _, p := range []string{"/", "/changes", "/system/health", "/nope"} {
		code, _, hdr := getPage(t, srv, p)
		if code != http.StatusOK && code != http.StatusNotFound {
			t.Fatalf("GET %s: status %d", p, code)
		}
		if hdr.Get("X-Frame-Options") != "DENY" || hdr.Get("Content-Security-Policy") != "frame-ancestors 'none'" {
			t.Fatalf("GET %s: X-Frame-Options=%q Content-Security-Policy=%q", p, hdr.Get("X-Frame-Options"), hdr.Get("Content-Security-Policy"))
		}
	}
}
