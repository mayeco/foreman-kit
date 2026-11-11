package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDotenvSetsNewKeysWithoutOverriding(t *testing.T) {
	dir := t.TempDir()
	content := "FOREMANKIT_DOTENV_NEW=from-file\nOPENSPEC_DOTENV_EXISTING=from-file\n# comment\nexport FOREMANKIT_DOTENV_EXPORTED=\"quoted value\"\n"
	if err := os.WriteFile(filepath.Join(dir, dotenvFile), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FOREMANKIT_DOTENV_EXISTING", "from-env")
	// Register cleanup for keys the loader sets so they do not leak into other tests.
	t.Setenv("FOREMANKIT_DOTENV_NEW", "")
	t.Setenv("FOREMANKIT_DOTENV_EXPORTED", "")
	os.Unsetenv("FOREMANKIT_DOTENV_NEW")
	os.Unsetenv("FOREMANKIT_DOTENV_EXPORTED")

	var stderr bytes.Buffer
	loadDotenv(dir, &stderr)

	if got := os.Getenv("FOREMANKIT_DOTENV_NEW"); got != "from-file" {
		t.Errorf("FOREMANKIT_DOTENV_NEW = %q, want %q", got, "from-file")
	}
	if got := os.Getenv("FOREMANKIT_DOTENV_EXISTING"); got != "from-env" {
		t.Errorf("FOREMANKIT_DOTENV_EXISTING = %q, want existing value %q", got, "from-env")
	}
	if got := os.Getenv("FOREMANKIT_DOTENV_EXPORTED"); got != "quoted value" {
		t.Errorf("FOREMANKIT_DOTENV_EXPORTED = %q, want %q", got, "quoted value")
	}
	if stderr.Len() != 0 {
		t.Errorf("unexpected warning: %s", stderr.String())
	}
}

func TestLoadDotenvMissingFileIsSilent(t *testing.T) {
	var stderr bytes.Buffer
	loadDotenv(t.TempDir(), &stderr)
	if stderr.Len() != 0 {
		t.Errorf("unexpected warning for missing file: %s", stderr.String())
	}
}

func TestLoadDotenvMalformedFileWarns(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, dotenvFile), []byte("FOREMANKIT_DOTENV_BAD=\"unterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FOREMANKIT_DOTENV_BAD", "")
	os.Unsetenv("FOREMANKIT_DOTENV_BAD")

	var stderr bytes.Buffer
	loadDotenv(dir, &stderr)
	if !strings.Contains(stderr.String(), "cannot load") {
		t.Errorf("expected a load warning, got %q", stderr.String())
	}
}
