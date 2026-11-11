package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The start time of a lock's owner is read with ps while AcquireLock holds the writer connection inside
// its transaction. A ps that does not answer must be given up on: without a bound it held the database's
// write lock against every other process for as long as it hung.
func TestProcessStartUnixGivesUpOnAPsThatDoesNotAnswer(t *testing.T) {
	binDir := t.TempDir()
	// The shell is killed at the timeout; the sleep it started keeps the output pipe open after it.
	if err := os.WriteFile(filepath.Join(binDir, "ps"), []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	prev := processStartProbeTimeout
	processStartProbeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { processStartProbeTimeout = prev })

	// A PID no process has: on Linux /proc answers for a live one and ps is never asked.
	const nobody = 0x7ffffff0
	started := time.Now()
	if got := ProcessStartUnix(nobody); got != 0 {
		t.Fatalf("ProcessStartUnix = %d from a ps that never answered", got)
	}
	if took := time.Since(started); took > 3*time.Second {
		t.Fatalf("the lookup waited %s for a ps that does not answer", took.Round(time.Millisecond))
	}
}
