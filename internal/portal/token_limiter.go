package portal

import (
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	tokenFailureLimit        = 10
	tokenFailureWindow       = 5 * time.Minute
	tokenLimiterMaxAddresses = 4096
	tokenOverflowKey         = "*"
)

type tokenWindow struct {
	start      time.Time
	failures   int
	logged     bool
	lockLogged bool
}

// tokenLimiter throttles wrong portal tokens per remote IP over a fixed 5-minute window.
type tokenLimiter struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]*tokenWindow
}

func newTokenLimiter() *tokenLimiter {
	return &tokenLimiter{
		now:     time.Now,
		entries: make(map[string]*tokenWindow),
	}
}

func (l *tokenLimiter) clock() time.Time {
	if l != nil && l.now != nil {
		return l.now()
	}
	return time.Now()
}

func remoteIP(r *http.Request) string {
	if r == nil {
		return "unknown"
	}
	return normalizeRemoteIP(r.RemoteAddr)
}

func normalizeRemoteIP(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "unknown"
	}
	if host, port, err := net.SplitHostPort(addr); err == nil && port != "" && strings.TrimSpace(host) != "" {
		return strings.Trim(strings.TrimSpace(host), "[]")
	}
	return strings.Trim(addr, "[]")
}

func (l *tokenLimiter) locked(ip string) (time.Duration, bool) {
	if l == nil {
		return 0, false
	}
	ip = normalizeRemoteIP(ip)
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.clock()
	if w, exists := l.entries[ip]; exists {
		end := w.start.Add(tokenFailureWindow)
		if !now.Before(end) {
			delete(l.entries, ip)
		} else if w.failures >= tokenFailureLimit {
			rem := end.Sub(now)
			if rem > tokenFailureWindow {
				rem = tokenFailureWindow
			}
			return rem, true
		} else {
			return 0, false
		}
	}
	if w, exists := l.entries[tokenOverflowKey]; exists {
		end := w.start.Add(tokenFailureWindow)
		if !now.Before(end) {
			delete(l.entries, tokenOverflowKey)
			return 0, false
		}
		if w.failures >= tokenFailureLimit {
			rem := end.Sub(now)
			if rem > tokenFailureWindow {
				rem = tokenFailureWindow
			}
			return rem, true
		}
	}
	return 0, false
}

func (l *tokenLimiter) fail(ip string) {
	if l == nil {
		return
	}
	ip = normalizeRemoteIP(ip)
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.clock()
	if l.entries == nil {
		l.entries = make(map[string]*tokenWindow)
	}

	w, exists := l.entries[ip]
	if exists && !now.Before(w.start.Add(tokenFailureWindow)) {
		delete(l.entries, ip)
		w, exists = nil, false
	}
	if !exists {
		if len(l.entries) >= tokenLimiterMaxAddresses {
			l.pruneExpired(now)
		}
		if len(l.entries) >= tokenLimiterMaxAddresses {
			w, exists = l.entries[tokenOverflowKey]
			if exists && !now.Before(w.start.Add(tokenFailureWindow)) {
				delete(l.entries, tokenOverflowKey)
				w, exists = nil, false
			}
			if !exists {
				if len(l.entries) >= tokenLimiterMaxAddresses {
					l.dropOneUnlocked()
				}
				w = &tokenWindow{start: now}
				l.entries[tokenOverflowKey] = w
			}
		} else {
			w = &tokenWindow{start: now}
			l.entries[ip] = w
		}
	}

	w.failures++
	if !w.logged {
		w.logged = true
		log.Printf("portal: refused a wrong token from %s; further refusals from it are not logged for %s", ip, tokenFailureWindow)
	}
	if w.failures >= tokenFailureLimit && !w.lockLogged {
		w.lockLogged = true
		end := w.start.Add(tokenFailureWindow)
		log.Printf("portal: %s sent %d wrong tokens; it is refused until %s", ip, tokenFailureLimit, end.UTC().Format(time.RFC3339))
	}
}

func (l *tokenLimiter) pruneExpired(now time.Time) {
	for k, w := range l.entries {
		if !now.Before(w.start.Add(tokenFailureWindow)) {
			delete(l.entries, k)
		}
	}
}

func (l *tokenLimiter) dropOneUnlocked() {
	for k, w := range l.entries {
		if k != tokenOverflowKey && w.failures < tokenFailureLimit {
			delete(l.entries, k)
			return
		}
	}
	for k := range l.entries {
		if k != tokenOverflowKey {
			delete(l.entries, k)
			return
		}
	}
}

func retryAfterSeconds(d time.Duration) int {
	secs := int((d + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	maxSecs := int(tokenFailureWindow / time.Second)
	if secs > maxSecs {
		secs = maxSecs
	}
	return secs
}

func retryAfterHeader(d time.Duration) string {
	return strconv.Itoa(retryAfterSeconds(d))
}
