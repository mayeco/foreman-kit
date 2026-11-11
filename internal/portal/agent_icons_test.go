package portal

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/cycle"
)

// TestEveryKnownAgentHasItsOwnIcon: the agents page drew the same robot for all thirteen agents the kit
// knows. Each one has its own glyph now, every glyph is in the embedded sprite, and a custom agent
// keeps the generic bot.
func TestEveryKnownAgentHasItsOwnIcon(t *testing.T) {
	sprite, err := os.ReadFile(filepath.Join("..", "assets", "portal_ui", "icons.svg"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for agent := range cycle.SupportedAgents {
		name := agentIconName(agent)
		if name == "bot" {
			t.Errorf("agent %q has no icon of its own", agent)
		}
		if other, dup := seen[name]; dup {
			t.Errorf("agents %q and %q share the icon %q", agent, other, name)
		}
		seen[name] = agent
		if !strings.Contains(string(sprite), `id="`+name+`"`) {
			t.Errorf("icon %q of agent %q is not in icons.svg: run npm run build in web/portal", name, agent)
		}
	}
	if got := agentIconName("  Claude "); got != agentIconName("claude") {
		t.Errorf("agent names are matched ignoring case and spaces, got %q", got)
	}
	if got := agentIconName("my-custom-agent"); got != "bot" {
		t.Errorf("an unknown agent should keep the generic bot, got %q", got)
	}
}

func TestAgentsPageDrawsEachAgentWithItsIcon(t *testing.T) {
	srv, _, _ := setupTestPortal(t)
	// The page leaves out an engine this machine does not have and nothing used, unless it is asked
	// for all of them. Without show=all the count depended on what the machine had installed.
	code, body, _ := getPage(t, srv, "/agents?show=all")
	if code != 200 {
		t.Fatalf("/agents?show=all = %d", code)
	}
	rows := regexp.MustCompile(`<p class="font-mono font-semibold">([^<]+)</p>`).FindAllStringSubmatch(body, -1)
	if len(rows) < len(cycle.SupportedAgents) {
		t.Fatalf("/agents lists %d agents, want the %d the kit knows", len(rows), len(cycle.SupportedAgents))
	}
	for agent := range cycle.SupportedAgents {
		icon := `#` + agentIconName(agent) + `"`
		if logo := agentLogo(agent); logo != "" {
			icon = `src="/assets/agents/` + logo + `"`
		}
		i := strings.Index(body, `<p class="font-mono font-semibold">`+agent+`</p>`)
		if i < 0 {
			t.Errorf("/agents has no row for %s", agent)
			continue
		}
		if !strings.Contains(body[max(0, i-700):i], icon) {
			t.Errorf("the row of %s does not draw %s", agent, icon)
		}
	}
}

// TestIconSpriteIsRevalidatedNotCachedForADay: icons.svg was cached for 24 hours while portal.css and
// portal.js were not, so after an upgrade added icons to the sprite the pages named symbols the old
// copy lacked and drew empty squares (the agents page, for one). The sprite revalidates by ETag.
func TestIconSpriteIsRevalidatedNotCachedForADay(t *testing.T) {
	srv, _, _ := setupTestPortal(t)
	get := func(path, inm string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4567"+path, nil)
		req.Host, req.RemoteAddr = "127.0.0.1:4567", "127.0.0.1:12345"
		if inm != "" {
			req.Header.Set("If-None-Match", inm)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	first := get("/assets/icons.svg", "")
	if first.Code != 200 || first.Header().Get("Cache-Control") != "no-cache" || first.Header().Get("ETag") == "" {
		t.Fatalf("sprite: %d, Cache-Control %q, ETag %q", first.Code, first.Header().Get("Cache-Control"), first.Header().Get("ETag"))
	}
	if again := get("/assets/icons.svg", first.Header().Get("ETag")); again.Code != http.StatusNotModified || again.Body.Len() != 0 {
		t.Fatalf("an unchanged sprite should answer 304 with no body, got %d (%d bytes)", again.Code, again.Body.Len())
	}
	if stale := get("/assets/icons.svg", `"old"`); stale.Code != 200 || stale.Body.Len() == 0 {
		t.Fatalf("a changed sprite should be sent again, got %d", stale.Code)
	}
	if font := get("/assets/fonts/ibm-plex-sans-latin-400-normal.woff2", ""); font.Code != 200 || !strings.Contains(font.Header().Get("Cache-Control"), "max-age") {
		t.Fatalf("fonts keep their cache: %d, %q", font.Code, font.Header().Get("Cache-Control"))
	}
}

// TestEveryIconTheTemplatesNameIsInTheSprite: the sprite is built from the icons the templates name, and
// the build did not see the ones handed to a partial as (dict "icon" "name"): the panel of the setup form,
// the empty states of the inbox and the ideas were drawn with an empty gap. The same patterns as
// web/portal/build.mjs, checked against the committed sprite.
func TestEveryIconTheTemplatesNameIsInTheSprite(t *testing.T) {
	sprite, err := os.ReadFile(filepath.Join("..", "assets", "portal_ui", "icons.svg"))
	if err != nil {
		t.Fatal(err)
	}
	defined := map[string]bool{}
	for _, m := range regexp.MustCompile(`<symbol id="([a-z0-9-]+)"`).FindAllStringSubmatch(string(sprite), -1) {
		defined[m[1]] = true
	}
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`\{\{-?\s*icon[cs]?\s+"([a-z0-9-]+)"`),
		regexp.MustCompile(`\bico\('([a-z0-9-]+)'`),
		regexp.MustCompile(`Icon:\s*"([a-z0-9-]+)"`),
		regexp.MustCompile(`data-icon="([a-z0-9-]+)"`),
		regexp.MustCompile(`\bicon"\s+"([a-z0-9-]+)"`),
	}
	var sources []string
	for _, glob := range []string{"templates/*.html", "js/*"} {
		files, _ := filepath.Glob(filepath.Join("..", "assets", "portal_ui", glob))
		sources = append(sources, files...)
	}
	sources = append(sources, "page_helpers.go")
	for _, f := range sources {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, re := range patterns {
			for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
				if !defined[m[1]] {
					t.Errorf("%s names the icon %q, which is not in icons.svg (cd web/portal && npm run build)", filepath.Base(f), m[1])
				}
			}
		}
	}
}

// TestAgentLogosAreServedAndContained: the agents are drawn with the logo each vendor publishes on its
// own site, embedded under portal_ui/agents/. Every logo belongs to an agent the kit knows, is served
// with its type and a policy that runs nothing (they are third-party files), and no other path is.
func TestAgentLogosAreServedAndContained(t *testing.T) {
	if len(agentLogos) == 0 {
		t.Fatal("no agent logo is embedded")
	}
	for agent, file := range agentLogos {
		if _, ok := cycle.SupportedAgents[agent]; !ok {
			t.Errorf("logo %s is for %q, which is not an agent the kit knows", file, agent)
		}
	}
	srv, _, _ := setupTestPortal(t)
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4567"+path, nil)
		req.Host, req.RemoteAddr = "127.0.0.1:4567", "127.0.0.1:12345"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	for _, file := range agentLogos {
		rec := get("/assets/agents/" + file)
		if rec.Code != 200 || rec.Header().Get("Content-Type") != agentLogoType(file) {
			t.Errorf("%s: %d %q", file, rec.Code, rec.Header().Get("Content-Type"))
		}
		if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'none'") {
			t.Errorf("%s is served without a policy that blocks scripts", file)
		}
	}
	for _, bad := range []string{"/assets/agents/../icons.svg", "/assets/agents/nope.svg", "/assets/agents/claude.txt"} {
		if rec := get(bad); rec.Code == 200 {
			t.Errorf("%s should not be served", bad)
		}
	}
	if !strings.Contains(string(agentIcon("claude", "size-4")), `<img src="/assets/agents/claude.svg"`) {
		t.Errorf("claude should be drawn with its logo: %s", agentIcon("claude", "size-4"))
	}
	if got := string(agentIcon("my-custom-agent", "size-4")); !strings.Contains(got, "#bot\"") {
		t.Errorf("a custom agent keeps the bot glyph: %s", got)
	}
}
