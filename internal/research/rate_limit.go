package research

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	rateLimitBackoff  = 30 * time.Second
	rateLimitFailures = 3
	rateLimitPause    = 5 * time.Minute
)

// All brokers in this process share host cooldowns, including across agents
// and room switches. No timers or sleeping model turns are needed to enforce them.
var sharedRateLimits = newRateLimits(time.Now)

type rateLimits struct {
	mu    sync.Mutex
	hosts map[string]*hostRateLimit
	now   func() time.Time
}

type hostRateLimit struct {
	gate     chan struct{}
	failures int
	retryAt  time.Time
}

func newRateLimits(now func() time.Time) *rateLimits {
	return &rateLimits{hosts: make(map[string]*hostRateLimit), now: now}
}

func (l *rateLimits) host(name string) *hostRateLimit {
	l.mu.Lock()
	defer l.mu.Unlock()
	state := l.hosts[name]
	if state == nil {
		state = &hostRateLimit{gate: make(chan struct{}, 1)}
		l.hosts[name] = state
	}
	return state
}

type rateLimitError struct {
	host       string
	retryAt    time.Time
	exhausted  bool
	suppressed bool
}

func (e *rateLimitError) Error() string {
	message := fmt.Sprintf("HTTP 429: %s is rate-limited; do not retry this host before %s", e.host, e.retryAt.UTC().Format(time.RFC3339Nano))
	if e.suppressed {
		message += "; request was not sent because the shared cooldown is active"
	}
	if e.exhausted {
		message += "; repeated rate limits exhausted retries, use other sources or provide a final answer with limitations"
	}
	return message
}

// Wrap the transport rather than just the original URL so redirects cannot
// bypass a destination's cooldown. The host gate permits only one recovery
// probe at a time and is cancelable while another request is in flight.
type rateLimitedTransport struct {
	next   http.RoundTripper
	limits *rateLimits
}

func (t *rateLimitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := strings.ToLower(strings.TrimSuffix(req.URL.Hostname(), "."))
	state := t.limits.host(host)
	select {
	case state.gate <- struct{}{}:
		defer func() { <-state.gate }()
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	now := t.limits.now()
	if now.Before(state.retryAt) {
		return nil, &rateLimitError{host: host, retryAt: state.retryAt, exhausted: state.failures >= rateLimitFailures, suppressed: true}
	}
	response, err := t.next.RoundTrip(req)
	if err != nil {
		return response, err
	}
	if response.StatusCode != http.StatusTooManyRequests {
		if response.StatusCode >= 200 && response.StatusCode < 400 {
			state.failures = 0
			state.retryAt = time.Time{}
		}
		return response, nil
	}
	response.Body.Close()
	state.failures = min(state.failures+1, rateLimitFailures)
	delay := rateLimitBackoff << (state.failures - 1)
	if state.failures >= rateLimitFailures {
		delay = rateLimitPause
	}
	// A small positive jitter avoids synchronized probes from separate processes.
	delay += time.Duration(rand.Int64N(int64(5 * time.Second)))
	now = t.limits.now()
	state.retryAt = retryAfter(response.Header.Get("Retry-After"), now, delay)
	return nil, &rateLimitError{host: host, retryAt: state.retryAt, exhausted: state.failures >= rateLimitFailures}
}

func retryAfter(value string, now time.Time, minimum time.Duration) time.Time {
	retryAt := now.Add(minimum)
	value = strings.TrimSpace(value)
	if value == "" {
		return retryAt
	}
	if value[0] >= '0' && value[0] <= '9' && strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) < 0 {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || seconds > uint64((1<<63-1)/int64(time.Second)) {
			// A valid but enormous delta must never overflow into an immediate retry.
			return time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
		}
		if candidate := now.Add(time.Duration(seconds) * time.Second); candidate.After(retryAt) {
			return candidate
		}
	} else if candidate, err := http.ParseTime(value); err == nil && candidate.After(retryAt) {
		return candidate
	}
	return retryAt
}
