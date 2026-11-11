package orchestrator

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestRedactExcerptRedactsBeforeTruncating places the cut inside "API_TOKEN=": truncating first left
// "KEN=<secret>", which no longer matched the redaction pattern.
func TestRedactExcerptRedactsBeforeTruncating(t *testing.T) {
	text := "API_TOKEN=ghp_s3cr3tvalue\npostgres://app:hunter2@db:5432/app\nok"
	for maxChars := 20; maxChars < len(text); maxChars++ {
		got := RedactExcerpt(text, 40, maxChars)
		if strings.Contains(got, "ghp_s3cr3tvalue") || strings.Contains(got, "hunter2") {
			t.Fatalf("maxChars=%d leaked a secret: %q", maxChars, got)
		}
	}
	if got := RedactExcerpt(strings.Repeat("é", 50), 40, 11); !utf8.ValidString(got) {
		t.Fatalf("truncation split a UTF-8 character: %q", got)
	}
}

// TestBacklogItemFromMapReadsStringListFields verifies that depends_on and locks written as a string are
// kept, so dependency and lock scheduling guards still apply.
func TestBacklogItemFromMapReadsStringListFields(t *testing.T) {
	item := BacklogItemFromMap(map[string]any{
		"change_name": "add-billing-ui",
		"depends_on":  "add-billing-api",
		"locks":       []any{"billing", 3},
	})
	if len(item.DependsOn) != 1 || item.DependsOn[0] != "add-billing-api" {
		t.Fatalf("depends_on lost: %+v", item.DependsOn)
	}
	if len(item.Locks) != 2 || item.Locks[0] != "billing" || item.Locks[1] != "3" {
		t.Fatalf("locks lost: %+v", item.Locks)
	}
}
