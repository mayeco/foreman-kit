package portal

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"
)

func TestTokenLimiterLocksAtLimitAndResetsAfterWindow(t *testing.T) {
	origOut := log.Writer()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(origOut)

	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	lim := newTokenLimiter()
	lim.now = func() time.Time { return now }

	const ip = "192.168.4.7"

	// Failures 1 through 9 do not lock the address.
	for i := 1; i < tokenFailureLimit; i++ {
		lim.fail(ip)
		if retryAfter, ok := lim.locked(ip); ok {
			t.Fatalf("locked after %d failures (retryAfter=%s), want unlocked until %d", i, retryAfter, tokenFailureLimit)
		}
		now = now.Add(time.Second)
	}

	// The 10th failure locks the address for the remainder of the 5-minute window.
	lim.fail(ip)
	retryAfter, ok := lim.locked(ip)
	if !ok {
		t.Fatalf("expected %s to be locked after %d failures", ip, tokenFailureLimit)
	}
	if retryAfter <= 0 || retryAfter > tokenFailureWindow {
		t.Fatalf("retryAfter = %s, want (0, %s]", retryAfter, tokenFailureWindow)
	}

	// Distinct addresses are independent.
	if _, otherLocked := lim.locked("192.168.4.8"); otherLocked {
		t.Fatalf("expected distinct address 192.168.4.8 to remain unlocked")
	}
	lim.fail("192.168.4.8")
	if _, otherLocked := lim.locked("192.168.4.8"); otherLocked {
		t.Fatalf("expected 192.168.4.8 to remain unlocked after 1 failure")
	}

	// Advancing to the end of the 5-minute window unlocks the address and resets its count.
	now = time.Date(2026, 11, 13, 12, 5, 0, 0, time.UTC)
	if _, stillLocked := lim.locked(ip); stillLocked {
		t.Fatalf("expected %s to unlock when the 5-minute window ends", ip)
	}
	lim.fail(ip)
	if _, lockedAfterReset := lim.locked(ip); lockedAfterReset {
		t.Fatalf("expected %s count to have reset after window expiry, but locked after 1 failure", ip)
	}

	// Verify log lines name only the address.
	logText := buf.String()
	if !strings.Contains(logText, ip) {
		t.Fatalf("expected log output to mention %s, got:\n%s", ip, logText)
	}
}

func TestTokenLimiterAddressCapAndOverflowBucket(t *testing.T) {
	origOut := log.Writer()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(origOut)

	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	lim := newTokenLimiter()
	lim.now = func() time.Time { return now }

	// Flood with more than tokenLimiterMaxAddresses distinct addresses within one window.
	total := tokenLimiterMaxAddresses + 500
	for i := 0; i < total; i++ {
		addr := fmt.Sprintf("10.%d.%d.%d", (i>>16)&0xff, (i>>8)&0xff, i&0xff)
		lim.fail(addr)
		if got := len(lim.entries); got > tokenLimiterMaxAddresses {
			t.Fatalf("after %d distinct addresses, len(entries) = %d, exceeds cap %d", i+1, got, tokenLimiterMaxAddresses)
		}
	}

	if got := len(lim.entries); got > tokenLimiterMaxAddresses {
		t.Fatalf("len(entries) = %d, want <= %d", got, tokenLimiterMaxAddresses)
	}
	// Overflow bucket should now be locked because 500 overflow failures >= tokenFailureLimit.
	if retryAfter, ok := lim.locked("172.16.99.99"); !ok || retryAfter <= 0 || retryAfter > tokenFailureWindow {
		t.Fatalf("expected new address to be throttled by overflow bucket: locked=%v retryAfter=%s", ok, retryAfter)
	}

	// After the window expires, pruning drops expired entries so new addresses get their own buckets again.
	now = now.Add(tokenFailureWindow)
	lim.fail("172.16.99.99")
	if _, ok := lim.locked("172.16.99.99"); ok {
		t.Fatalf("expected 172.16.99.99 not to be locked after 1 failure in a fresh window")
	}
	if got := len(lim.entries); got != 1 {
		t.Fatalf("expected expired entries to be pruned on overflow check or reset, len(entries) = %d", got)
	}
}
