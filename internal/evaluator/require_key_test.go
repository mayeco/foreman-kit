package evaluator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePolicy(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".foreman-kit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".foreman-kit", "evaluator.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func clearKeyEnv(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "")
}

func TestRequireAPIKeyForActiveGates(t *testing.T) {
	cases := []struct {
		name    string
		policy  string
		key     string
		wantErr bool
	}{
		{"advisory without key fails", "gates:\n  plan_review:\n    mode: advisory\n", "", true},
		{"enforce without key fails", "gates:\n  change_risk:\n    mode: enforce\n", "", true},
		{"all off without key passes", "gates:\n  plan_review:\n    mode: off\n  change_risk:\n    mode: off\n", "", false},
		{"active with key passes", "gates:\n  plan_review:\n    mode: enforce\n", "ts-key", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearKeyEnv(t)
			t.Setenv("TYPESAFE_API_KEY", tc.key)
			pol, err := LoadPolicy(writePolicy(t, tc.policy))
			if err != nil {
				t.Fatal(err)
			}
			err = pol.RequireAPIKeyForActiveGates()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "TYPESAFE_API_KEY") {
				t.Fatalf("error should name TYPESAFE_API_KEY: %v", err)
			}
		})
	}
}

func TestRequireAPIKeyIgnoresCredentialsFile(t *testing.T) {
	clearKeyEnv(t)
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	_ = os.MkdirAll(filepath.Join(xdg, "jev"), 0o755)
	_ = os.WriteFile(filepath.Join(xdg, "jev", "credentials.json"), []byte(`{"api_key":"file-key"}`), 0o600)
	pol, err := LoadPolicy(writePolicy(t, "gates:\n  plan_review:\n    mode: advisory\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := pol.RequireAPIKeyForActiveGates(); err == nil {
		t.Fatal("a credentials file must not satisfy the env requirement")
	}
}
