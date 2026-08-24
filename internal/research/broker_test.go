package research

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
)

type staticResolver struct {
	ips []net.IP
	err error
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func (r staticResolver) LookupIP(context.Context, string, string) ([]net.IP, error) {
	return append([]net.IP(nil), r.ips...), r.err
}

func TestPublicDestinationValidationBlocksPrivateSpecialAndMixedDNS(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "10.0.0.1", "100.64.0.1", "169.254.169.254", "172.16.0.1", "192.168.1.1",
		"192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "::1", "fc00::1", "fe80::1", "2001:db8::1",
	}
	for _, value := range blocked {
		if publicIP(net.ParseIP(value)) {
			t.Errorf("special address accepted: %s", value)
		}
	}
	for _, value := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if !publicIP(net.ParseIP(value)) {
			t.Errorf("public address rejected: %s", value)
		}
	}
	_, err := resolvePublic(context.Background(), staticResolver{ips: []net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("127.0.0.1")}}, "rebind.example")
	if err == nil || !strings.Contains(err.Error(), "private or special-purpose") {
		t.Fatalf("mixed public/private DNS was not rejected: %v", err)
	}
}

func TestPublicURLValidationRequiresCredentialFreeHTTPS443(t *testing.T) {
	values := []string{
		"http://example.com", "https://user:pass@example.com", "https://example.com:8443", "https:///missing-host",
	}
	for _, value := range values {
		u, _ := url.Parse(value)
		if err := validatePublicURL(u); err == nil {
			t.Errorf("unsafe URL accepted: %s", value)
		}
	}
	u, _ := url.Parse("https://example.com/docs?q=go")
	if err := validatePublicURL(u); err != nil {
		t.Fatalf("public HTTPS URL rejected: %v", err)
	}
	broker := &Broker{}
	if _, _, _, err := broker.open(context.Background(), "https://example.com/"+strings.Repeat("x", 4096)); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized URL was not rejected: %v", err)
	}
}

func TestSearchAndPageExtractionAreBoundedAndIgnoreExecutableContent(t *testing.T) {
	searchDocument, err := html.Parse(strings.NewReader(`<html><body>
<div data-type="web"><a class="l1" href="https://go.dev/doc/"><div class="search-snippet-title">Go Documentation</div></a><div class="generic-snippet"><script>ignore()</script>Official docs</div></div>
<div data-type="web"><a class="l1" href="http://private.invalid"><div class="search-snippet-title">Unsafe</div></a></div>
</body></html>`))
	if err != nil {
		t.Fatal(err)
	}
	hits := parseSearchHits(searchDocument)
	if len(hits) != 1 || hits[0].Title != "Go Documentation" || hits[0].URL != "https://go.dev/doc/" || !strings.Contains(hits[0].Snippet, "Official docs") {
		t.Fatalf("hits=%+v", hits)
	}

	page, err := html.Parse(strings.NewReader(`<html><head><title> Example Docs </title><style>secret-style</style></head><body><h1>Hello</h1><script>secret-script</script><p>World</p></body></html>`))
	if err != nil {
		t.Fatal(err)
	}
	title, content := pageText(page)
	if title != "Example Docs" || !strings.Contains(content, "Hello World") || strings.Contains(content, "secret-") {
		t.Fatalf("title=%q content=%q", title, content)
	}
}

func TestResearchAuditStoresHashesNotRawInputs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "research_audit.jsonl")
	broker := New(path)
	secretURL := "https://127.0.0.1/private?token=do-not-log"
	results := broker.Research(context.Background(), chat.Codex, "room", []agent.ResearchRequest{{Type: "open", URL: secretURL}})
	if len(results) != 1 || results[0].Error == "" {
		t.Fatalf("private URL was not blocked: %+v", results)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secretURL) || strings.Contains(string(data), "do-not-log") || !strings.Contains(string(data), "input_sha256") {
		t.Fatalf("audit exposed raw input or omitted hash: %s", data)
	}
	if !strings.Contains(string(data), `"outcome":"requested"`) || !strings.Contains(string(data), `"outcome":"rejected"`) {
		t.Fatalf("audit omitted request lifecycle: %s", data)
	}
}

func TestResearchFailsClosedBeforeNetworkWhenAuditIsUnavailable(t *testing.T) {
	directory := t.TempDir()
	blocker := filepath.Join(directory, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	broker := New(filepath.Join(blocker, "audit.jsonl"))
	broker.client.Transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("network request occurred without an audit record")
		return nil, nil
	})
	results := broker.Research(context.Background(), chat.Codex, "room", []agent.ResearchRequest{{Type: "open", URL: "https://example.com"}})
	if len(results) != 1 || !strings.Contains(results[0].Error, "request was not sent") {
		t.Fatalf("results=%+v", results)
	}
}

func TestAuditErrorNeverReturnsRawFailureText(t *testing.T) {
	secret := `parse "https://example.com/?token=do-not-log": invalid URL escape "%zz"`
	if got := auditError(secret); strings.Contains(got, "do-not-log") || got != "invalid_input" {
		t.Fatalf("auditError=%q", got)
	}
}

func TestFetchUsesCredentialFreeGETAndEnforcesSizeAndContent(t *testing.T) {
	for name, test := range map[string]struct {
		body        string
		contentType string
		wantError   string
	}{
		"oversize":     {strings.Repeat("x", maxResponseBytes+1), "text/plain", "exceeds"},
		"binary":       {"binary", "application/octet-stream", "unsupported content type"},
		"bounded text": {"hello", "text/plain", ""},
	} {
		t.Run(name, func(t *testing.T) {
			client := secureClient(staticResolver{ips: []net.IP{net.ParseIP("1.1.1.1")}})
			client.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				if request.Method != http.MethodGet || request.Body != nil || request.Header.Get("Authorization") != "" || len(request.Cookies()) != 0 {
					t.Fatalf("unsafe request: method=%s body=%v headers=%v", request.Method, request.Body, request.Header)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{test.contentType}},
					Body:       io.NopCloser(strings.NewReader(test.body)),
					Request:    request,
				}, nil
			})
			broker := &Broker{client: client}
			_, _, _, err := broker.open(context.Background(), "https://example.com/docs")
			if test.wantError == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("err=%v want substring %q", err, test.wantError)
			}
		})
	}
}

func TestRedirectValidationRejectsDowngradeAndNonstandardPort(t *testing.T) {
	for _, location := range []string{"http://example.com/next", "https://example.com:8443/next"} {
		client := secureClient(staticResolver{ips: []net.IP{net.ParseIP("1.1.1.1")}})
		client.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{location}},
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    request,
			}, nil
		})
		broker := &Broker{client: client}
		if _, _, _, err := broker.open(context.Background(), "https://example.com/start"); err == nil {
			t.Fatalf("unsafe redirect accepted: %s", location)
		}
	}
}

func TestLivePublicSearchAndOpen(t *testing.T) {
	if os.Getenv("MOHUDDLE_TEST_WEB_RESEARCH") != "1" {
		t.Skip("set MOHUDDLE_TEST_WEB_RESEARCH=1 for the live public-web smoke test")
	}
	broker := New(filepath.Join(t.TempDir(), "audit.jsonl"))
	search := broker.Research(context.Background(), chat.Codex, "live", []agent.ResearchRequest{{Type: "search", Query: "Go context package documentation"}})
	if len(search) != 1 || search[0].Error != "" || len(search[0].Hits) == 0 {
		t.Fatalf("live search=%+v", search)
	}
	opened := broker.Research(context.Background(), chat.Codex, "live", []agent.ResearchRequest{{Type: "open", URL: search[0].Hits[0].URL}})
	if len(opened) != 1 || opened[0].Error != "" || strings.TrimSpace(opened[0].Content) == "" {
		t.Fatalf("live open=%+v", opened)
	}
}
