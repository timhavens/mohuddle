package research

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
)

func researchResponse(req *http.Request, status int, headers http.Header) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Content-Type", "text/plain")
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader("official source text")), Request: req}
}

func TestRetryAfterHonorsServerWaitWithoutOverflow(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		header string
		want   time.Time
	}{
		{"120", now.Add(2 * time.Minute)},
		{now.Add(time.Hour).Format(http.TimeFormat), now.Add(time.Hour)},
		{"", now.Add(30 * time.Second)},
		{"invalid", now.Add(30 * time.Second)},
		{"-60", now.Add(30 * time.Second)},
		{"0", now.Add(30 * time.Second)},
		{now.Add(-time.Hour).Format(http.TimeFormat), now.Add(30 * time.Second)},
		{"18446744073709551616", time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)},
		{"9223372036854775807", time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)},
	} {
		t.Run(tc.header, func(t *testing.T) {
			if got := retryAfter(tc.header, now, 30*time.Second); !got.Equal(tc.want) {
				t.Fatalf("retry time=%v want %v", got, tc.want)
			}
		})
	}
}

func TestResearchSharesCooldownAcrossAgentsQueriesAndRedirects(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	limits := newRateLimits(func() time.Time { return now })
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	first, second := New(path), New(path)
	if first.limits != second.limits {
		t.Fatal("new brokers do not share process cooldowns")
	}
	counts := map[string]int{}
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		counts[req.URL.Hostname()]++
		switch req.URL.Hostname() {
		case "search.brave.com":
			return researchResponse(req, 429, http.Header{"Retry-After": {"120"}}), nil
		case "redirect.example.com":
			return researchResponse(req, 302, http.Header{"Location": {"https://search.brave.com/another-query"}}), nil
		default:
			return researchResponse(req, 200, nil), nil
		}
	})
	for _, broker := range []*Broker{first, second} {
		broker.limits = limits
		broker.client.Transport = transport
	}
	result := first.Research(context.Background(), chat.Codex, "one", []agent.ResearchRequest{{Type: "search", Query: "secret-query-first"}})[0]
	if result.ErrorCode != agent.ResearchRateLimited || result.StatusCode != 429 || result.Host != "search.brave.com" || result.RetryAt == nil || !result.RetryAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("missing rate-limit metadata: %+v", result)
	}
	results := second.Research(context.Background(), chat.Claude, "two", []agent.ResearchRequest{
		{Type: "search", Query: "secret-query-reworded"},
		{Type: "open", URL: "https://SEARCH.BRAVE.COM./different-path"},
		{Type: "open", URL: "https://redirect.example.com/docs"},
		{Type: "open", URL: "https://docs.example.com/official"},
	})
	for _, result := range results[:3] {
		if result.ErrorCode != agent.ResearchCooldown || result.Host != "search.brave.com" || result.RetryAt == nil || !strings.Contains(result.Error, "request was not sent") {
			t.Fatalf("cooldown was not enforced: %+v", result)
		}
	}
	if results[3].Error != "" || results[3].Content != "official source text" || counts["search.brave.com"] != 1 || counts["docs.example.com"] != 1 || counts["redirect.example.com"] != 1 || len(counts) != 3 {
		t.Fatalf("traffic not isolated to available hosts: counts=%v results=%+v", counts, results)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret-query", "/different-path", "/official", "/docs"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("raw input in audit: %s", data)
		}
	}
	for _, field := range []string{`"outcome":"rate_limited"`, `"outcome":"rate_limit_cooldown"`, `"status_code":429`, `"retry_at":`, `"host":"search.brave.com"`} {
		if !strings.Contains(string(data), field) {
			t.Fatalf("audit missing %s: %s", field, data)
		}
	}
}

func TestResearchRepeatedRateLimitsBackOffAndRecover(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	broker := New(filepath.Join(t.TempDir(), "audit.jsonl"))
	broker.limits = newRateLimits(func() time.Time { return now })
	calls, status := 0, 429
	broker.client.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return researchResponse(req, status, nil), nil
	})
	request := []agent.ResearchRequest{{Type: "open", URL: "https://example.com/docs"}}
	for i, minimum := range []time.Duration{30 * time.Second, 60 * time.Second, 5 * time.Minute} {
		result := broker.Research(context.Background(), chat.Codex, "room", request)[0]
		if result.ErrorCode != agent.ResearchRateLimited || result.RetryAt == nil || result.RetryExhausted != (i == 2) {
			t.Fatalf("attempt %d: %+v", i+1, result)
		}
		if delay := result.RetryAt.Sub(now); delay < minimum || delay >= minimum+5*time.Second {
			t.Fatalf("attempt %d delay=%v", i+1, delay)
		}
		blocked := broker.Research(context.Background(), chat.Claude, "room", request)[0]
		if blocked.ErrorCode != agent.ResearchCooldown || calls != i+1 {
			t.Fatalf("attempt %d cooldown sent another request: calls=%d result=%+v", i+1, calls, blocked)
		}
		now = *result.RetryAt
	}
	status = 200
	if result := broker.Research(context.Background(), chat.Codex, "new-task", request)[0]; result.Error != "" || calls != 4 {
		t.Fatalf("recovery probe failed: calls=%d result=%+v", calls, result)
	}
	status = 429
	result := broker.Research(context.Background(), chat.Codex, "new-task", request)[0]
	if result.RetryAt == nil || result.RetryExhausted || result.RetryAt.Sub(now) < 30*time.Second || result.RetryAt.Sub(now) >= 35*time.Second {
		t.Fatalf("successful response did not reset backoff: %+v", result)
	}
}

func TestResearchConcurrentAgentsSendOnlyOneThrottledProbe(t *testing.T) {
	broker := New(filepath.Join(t.TempDir(), "audit.jsonl"))
	broker.limits = newRateLimits(time.Now)
	var calls atomic.Int32
	broker.client.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return researchResponse(req, 429, http.Header{"Retry-After": {"3600"}}), nil
	})
	var wg sync.WaitGroup
	results := make(chan agent.ResearchResult, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results <- broker.Research(context.Background(), chat.Codex, "room", []agent.ResearchRequest{{Type: "open", URL: fmt.Sprintf("https://example.com/page-%d", i)}})[0]
		}(i)
	}
	wg.Wait()
	close(results)
	actual, suppressed := 0, 0
	for result := range results {
		switch result.ErrorCode {
		case agent.ResearchRateLimited:
			actual++
		case agent.ResearchCooldown:
			suppressed++
		default:
			t.Fatalf("unexpected result: %+v", result)
		}
	}
	if calls.Load() != 1 || actual != 1 || suppressed != 11 {
		t.Fatalf("network calls=%d actual=%d suppressed=%d", calls.Load(), actual, suppressed)
	}
}

func TestResearchHostGateRespectsCancellation(t *testing.T) {
	broker := New(filepath.Join(t.TempDir(), "audit.jsonl"))
	broker.limits = newRateLimits(time.Now)
	started, release := make(chan struct{}), make(chan struct{})
	broker.client.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		close(started)
		<-release
		return researchResponse(req, 200, nil), nil
	})
	done := make(chan error, 1)
	go func() {
		_, _, _, err := broker.open(context.Background(), "https://example.com/first")
		done <- err
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, err := broker.open(ctx, "https://example.com/canceled")
	close(release)
	if firstErr := <-done; firstErr != nil {
		t.Fatal(firstErr)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled gate wait=%v", err)
	}
}
