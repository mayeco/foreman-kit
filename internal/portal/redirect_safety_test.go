package portal

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The canonical-address redirect (a trailing slash removed) must only ever point at a page of the
// portal itself. A path such as //evil.example/ trimmed to //evil.example is a protocol-relative URL,
// and browsers read /\evil.example the same way: either would send whoever follows a crafted portal
// link to another site.
func TestPortalTrailingSlashRedirectNeverLeavesThePortal(t *testing.T) {
	srv, _, _ := setupTestPortal(t)
	origin, _ := url.Parse("http://127.0.0.1:4567/")

	for _, p := range []string{
		"//evil.example/", "///evil.example/", "//evil.example//", `/\evil.example/`, `/\/evil.example/`, "//evil.example/?q=1",
		"/%5Cevil.example/", "/%2F/evil.example/", "/%09/evil.example/", "/%0D%0A/evil.example/", "/%0A/evil.example/", "/x/%09/",
	} {
		code, _, hdr := getPage(t, srv, p)
		loc := hdr.Get("Location")
		if loc == "" {
			if code == http.StatusOK {
				t.Fatalf("GET %s: unexpected page", p)
			}
			continue
		}
		// Read the Location the way a browser does: tabs and newlines dropped, a backslash as a slash.
		browser := strings.ReplaceAll(strings.Map(func(c rune) rune {
			if c == '\t' || c == '\n' || c == '\r' {
				return -1
			}
			return c
		}, loc), `\`, "/")
		u, err := url.Parse(browser)
		if err != nil || strings.HasPrefix(browser, "//") || origin.ResolveReference(u).Host != origin.Host {
			t.Fatalf("GET %s: redirects off the portal to %q", p, loc)
		}
	}

	// A plain trailing slash still redirects to the canonical address, keeping its encoding.
	for p, want := range map[string]string{"/changes/?q=a": "/changes?q=a", "/changes/a%2Fb/?q=1": "/changes/a%2Fb?q=1"} {
		code, _, hdr := getPage(t, srv, p)
		if code != http.StatusMovedPermanently || hdr.Get("Location") != want {
			t.Fatalf("GET %s: want a redirect to %s, got %d %q", p, want, code, hdr.Get("Location"))
		}
	}
}
