package portal

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/assets"
)

// TestTheMenuOffersTheThemesTheStyleSheetHolds: the menu of themes, the source of the style sheet
// and the style sheet that was built from it name the same themes. A theme the menu offers and the
// style sheet lacks draws the page with no colors of its own; one the style sheet holds and the
// menu does not offer is weight nobody can choose, and a theme no browser test draws. The menu
// says of each whether it is dark, and it offers a light one and a dark one at least.
func TestTheMenuOffersTheThemesTheStyleSheetHolds(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "web", "portal", "input.css"))
	if err != nil {
		t.Fatal(err)
	}
	built, err := fs.ReadFile(assets.FS, "portal_ui/portal.css")
	if err != nil {
		t.Fatal(err)
	}

	// The source names the themes of daisyUI it takes in one list, and each theme of its own in a block.
	var inSource []string
	list := regexp.MustCompile(`(?s)@plugin "daisyui" \{.*?\bthemes:\s*([^;]+);`).FindSubmatch(source)
	if list == nil {
		t.Fatal("web/portal/input.css no longer lists the themes of daisyUI the way this test reads them")
	}
	for _, name := range strings.Split(string(list[1]), ",") {
		inSource = append(inSource, strings.TrimSpace(name))
	}
	own := regexp.MustCompile(`(?s)@plugin "daisyui/theme" \{\s*name:\s*"([^"]+)";(.*?)\n\}`).FindAllSubmatch(source, -1)
	if len(own) == 0 {
		t.Fatal("web/portal/input.css no longer declares the theme of the kit the way this test reads it")
	}
	for _, theme := range own {
		name := string(theme[1])
		inSource = append(inSource, name)
		if isDefault := strings.Contains(string(theme[2]), "default: true;"); isDefault != (name == defaultTheme) {
			t.Errorf("the style sheet says the theme %s is the default one: %v, and the portal draws %s when a browser chose none", name, isDefault, defaultTheme)
		}
	}

	var inMenu []string
	light, dark := 0, 0
	for _, theme := range portalThemes {
		inMenu = append(inMenu, theme.Name)
		scheme := regexp.MustCompile(`\[data-theme=` + regexp.QuoteMeta(theme.Name) + `\]\{color-scheme:(light|dark)`).FindSubmatch(built)
		switch {
		case scheme == nil:
			t.Errorf("the menu offers the theme %s, and the style sheet that was built does not hold it: run `npm run build` in web/portal", theme.Name)
		case (string(scheme[1]) == "dark") != theme.Dark:
			t.Errorf("the menu says the theme %s is dark: %v, and the style sheet draws it %s", theme.Name, theme.Dark, scheme[1])
		}
		if theme.Dark {
			dark++
		} else {
			light++
		}
	}
	if light == 0 || dark == 0 {
		t.Errorf("the menu offers %d light theme(s) and %d dark one(s): it offers one of each at least", light, dark)
	}
	if !validTheme(defaultTheme) {
		t.Errorf("the menu does not offer %s, the theme the portal draws when a browser chose none", defaultTheme)
	}

	var inBuilt []string
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`\[data-theme=([a-z0-9-]+)\]\{color-scheme:`).FindAllSubmatch(built, -1) {
		if name := string(m[1]); !seen[name] {
			seen[name] = true
			inBuilt = append(inBuilt, name)
		}
	}

	sort.Strings(inSource)
	sort.Strings(inMenu)
	sort.Strings(inBuilt)
	if strings.Join(inMenu, " ") != strings.Join(inSource, " ") {
		t.Errorf("the menu offers\n  %s\nand web/portal/input.css holds\n  %s", strings.Join(inMenu, " "), strings.Join(inSource, " "))
	}
	if strings.Join(inBuilt, " ") != strings.Join(inSource, " ") {
		t.Errorf("the style sheet that was built holds\n  %s\nand web/portal/input.css holds\n  %s\nrun `npm run build` in web/portal", strings.Join(inBuilt, " "), strings.Join(inSource, " "))
	}
}
