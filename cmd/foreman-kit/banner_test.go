package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

// What a program reads is never touched by the banner: a writer that is not a terminal gets none.
func TestBannerIsSilentOutsideATerminal(t *testing.T) {
	var buf bytes.Buffer
	writeBanner(&buf)
	writeBannerLine(&buf, "status")
	if buf.Len() != 0 {
		t.Fatalf("banner written to a writer that is not a terminal: %q", buf.String())
	}
	// Without a terminal the serving block is plain lines a log can keep.
	writeServing(&buf, []bannerRow{{"portal", "http://127.0.0.1:4567"}})
	if got := buf.String(); got != "foreman-kit: portal: http://127.0.0.1:4567\n" {
		t.Fatalf("serving block = %q", got)
	}
}

func TestWantsBanner(t *testing.T) {
	cases := []struct {
		path, args []string
		want       bool
	}{
		{[]string{"status"}, nil, true},
		{[]string{"progress"}, []string{"--json"}, false},
		{[]string{"exec"}, []string{"web", "make"}, false},
		{[]string{"agent-exec"}, nil, false},
		{[]string{"completed-since"}, []string{"abc"}, false},
		{[]string{"daemon"}, []string{"status"}, true},
	}
	for _, c := range cases {
		if got := wantsBanner(c.path, c.args); got != c.want {
			t.Errorf("wantsBanner(%v, %v) = %v, want %v", c.path, c.args, got, c.want)
		}
	}
}

func TestGradientEndsAtTheLastStop(t *testing.T) {
	if at(0) != bannerStops[0] || at(1) != bannerStops[len(bannerStops)-1] {
		t.Fatalf("gradient ends: %v %v", at(0), at(1))
	}
}

func TestIsApplication(t *testing.T) {
	dir := t.TempDir()
	if store.IsApplication(dir) {
		t.Fatal("an empty directory is not an application")
	}
	if err := os.MkdirAll(filepath.Join(dir, ".foreman-kit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".foreman-kit", "kit.lock"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !store.IsApplication(dir) {
		t.Fatal("a directory with .foreman-kit/kit.lock is an application")
	}
}
