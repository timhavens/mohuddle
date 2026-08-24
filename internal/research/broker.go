package research

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/net/html"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
)

const (
	maxRequests      = 4
	maxSearchHits    = 8
	maxResponseBytes = 1024 * 1024
	maxPageTextBytes = 64 * 1024
	maxRedirects     = 5
	requestTimeout   = 15 * time.Second
	searchEndpoint   = "https://search.brave.com/search"
)

type resolver interface {
	LookupIP(context.Context, string, string) ([]net.IP, error)
}

type Broker struct {
	client *http.Client
	audit  *auditLog
}

// New constructs a credential-free public-web broker. The HTTP transport does
// not inherit proxy, cookie, or credential state from the environment.
func New(auditPath string) *Broker {
	b := &Broker{audit: &auditLog{path: auditPath}}
	b.client = secureClient(net.DefaultResolver)
	return b
}

func secureClient(resolver resolver) *http.Client {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, fmt.Errorf("parse destination: %w", err)
			}
			if port != "443" {
				return nil, fmt.Errorf("public research permits HTTPS port 443 only")
			}
			ips, err := resolvePublic(ctx, resolver, host)
			if err != nil {
				return nil, err
			}
			dialer := &net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
		},
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	}
	client := &http.Client{Transport: transport, Timeout: requestTimeout}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("too many redirects")
		}
		return validatePublicURL(req.URL)
	}
	return client
}

// Research executes a small batch of typed, read-only requests. Each request
// yields its own result so a failed page cannot hide successful sources.
func (b *Broker) Research(ctx context.Context, participant chat.Participant, roomID string, requests []agent.ResearchRequest) []agent.ResearchResult {
	if len(requests) > maxRequests {
		requests = requests[:maxRequests]
	}
	results := make([]agent.ResearchResult, 0, len(requests))
	for _, request := range requests {
		baseRecord := auditRecord{
			Participant: participant,
			RoomID:      roomID,
			Type:        strings.ToLower(strings.TrimSpace(request.Type)),
			InputHash:   requestHash(request),
		}
		attempt := baseRecord
		attempt.Outcome = "requested"
		if err := b.audit.append(attempt); err != nil {
			results = append(results, agent.ResearchResult{Type: baseRecord.Type, Error: "host web research audit is unavailable; request was not sent"})
			continue
		}
		result := b.researchOne(ctx, request)
		completed := baseRecord
		completed.Type = result.Type
		completed.Host = resultHost(result)
		completed.Outcome = "succeeded"
		if result.Error != "" {
			completed.Outcome = "rejected"
			completed.Error = auditError(result.Error)
		}
		if err := b.audit.append(completed); err != nil {
			result = agent.ResearchResult{Type: result.Type, Error: "host web research outcome audit failed; results were withheld"}
		}
		results = append(results, result)
	}
	return results
}

func (b *Broker) researchOne(ctx context.Context, request agent.ResearchRequest) agent.ResearchResult {
	typeName := strings.ToLower(strings.TrimSpace(request.Type))
	switch typeName {
	case "search":
		query := strings.TrimSpace(request.Query)
		result := agent.ResearchResult{Type: typeName, Query: query}
		if query == "" {
			result.Error = "search query is required"
			return result
		}
		if len(query) > 500 {
			result.Error = "search query exceeds 500 characters"
			return result
		}
		hits, err := b.search(ctx, query)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		result.Hits = hits
		return result
	case "open":
		rawURL := strings.TrimSpace(request.URL)
		result := agent.ResearchResult{Type: typeName, URL: rawURL}
		title, content, finalURL, err := b.open(ctx, rawURL)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		result.URL, result.Title, result.Content = finalURL, title, content
		return result
	default:
		return agent.ResearchResult{Type: typeName, Error: "research type must be search or open"}
	}
}

func (b *Broker) search(ctx context.Context, query string) ([]agent.ResearchHit, error) {
	u, _ := url.Parse(searchEndpoint)
	values := u.Query()
	values.Set("q", query)
	values.Set("source", "web")
	u.RawQuery = values.Encode()
	body, contentType, _, err := b.fetch(ctx, u)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(contentType, "text/html") && !strings.Contains(contentType, "application/xhtml+xml") {
		return nil, fmt.Errorf("search returned unsupported content type %q", contentType)
	}
	document, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("parse search response: %w", err)
	}
	hits := parseSearchHits(document)
	if len(hits) == 0 {
		return nil, fmt.Errorf("search returned no public results")
	}
	return hits, nil
}

func (b *Broker) open(ctx context.Context, rawURL string) (string, string, string, error) {
	if len(rawURL) > 4096 {
		return "", "", "", fmt.Errorf("URL exceeds 4096 characters")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", "", fmt.Errorf("parse URL: %w", err)
	}
	if err := validatePublicURL(u); err != nil {
		return "", "", "", err
	}
	body, contentType, finalURL, err := b.fetch(ctx, u)
	if err != nil {
		return "", "", "", err
	}
	switch {
	case strings.Contains(contentType, "text/html"), strings.Contains(contentType, "application/xhtml+xml"):
		document, err := html.Parse(strings.NewReader(string(body)))
		if err != nil {
			return "", "", "", fmt.Errorf("parse page: %w", err)
		}
		title, text := pageText(document)
		return title, truncateBytes(text, maxPageTextBytes), finalURL, nil
	case strings.HasPrefix(contentType, "text/plain"), strings.Contains(contentType, "application/json"):
		return "", truncateBytes(normalizeSpace(string(body)), maxPageTextBytes), finalURL, nil
	default:
		return "", "", "", fmt.Errorf("unsupported content type %q", contentType)
	}
}

func (b *Broker) fetch(ctx context.Context, u *url.URL) ([]byte, string, string, error) {
	if err := validatePublicURL(u); err != nil {
		return nil, "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", "", err
	}
	req.Header.Set("User-Agent", "MoHuddle-Research/1.0 (+public read-only fetch)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain,application/json;q=0.8")
	req.Header.Set("Accept-Encoding", "identity")
	response, err := b.client.Do(req)
	if err != nil {
		var requestError *url.Error
		if errors.As(err, &requestError) && requestError.Err != nil {
			err = requestError.Err
		}
		return nil, "", "", fmt.Errorf("fetch public page: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, "", "", fmt.Errorf("public page returned HTTP %d", response.StatusCode)
	}
	reader := io.LimitReader(response.Body, maxResponseBytes+1)
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, "", "", fmt.Errorf("read public page: %w", err)
	}
	if len(body) > maxResponseBytes {
		return nil, "", "", fmt.Errorf("public page exceeds %d bytes", maxResponseBytes)
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	if contentType == "" {
		contentType = strings.ToLower(http.DetectContentType(body))
	}
	return body, contentType, response.Request.URL.String(), nil
}

func validatePublicURL(u *url.URL) error {
	if u == nil || !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("public research requires an HTTPS URL")
	}
	if u.User != nil {
		return fmt.Errorf("URL credentials are not allowed")
	}
	if strings.TrimSpace(u.Hostname()) == "" {
		return fmt.Errorf("URL hostname is required")
	}
	if port := u.Port(); port != "" && port != "443" {
		return fmt.Errorf("public research permits HTTPS port 443 only")
	}
	return nil
}

func resolvePublic(ctx context.Context, resolver resolver, host string) ([]net.IP, error) {
	if parsed := net.ParseIP(strings.Trim(host, "[]")); parsed != nil {
		if !publicIP(parsed) {
			return nil, fmt.Errorf("private or special-purpose destination is blocked")
		}
		return []net.IP{parsed}, nil
	}
	ips, err := resolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve public hostname: %w", err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("public hostname has no addresses")
	}
	for _, ip := range ips {
		if !publicIP(ip) {
			return nil, fmt.Errorf("hostname resolves to a private or special-purpose address")
		}
	}
	return ips, nil
}

var blockedPrefixes = mustPrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15",
	"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"::/128", "::1/128", "64:ff9b::/96", "100::/64", "2001:db8::/32", "fc00::/7", "fe80::/10", "ff00::/8",
)

func mustPrefixes(values ...string) []netip.Prefix {
	result := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		result = append(result, netip.MustParsePrefix(value))
	}
	return result
}

func publicIP(ip net.IP) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	if !address.IsValid() || !address.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func parseSearchHits(root *html.Node) []agent.ResearchHit {
	var hits []agent.ResearchHit
	var visit func(*html.Node)
	visit = func(node *html.Node) {
		if len(hits) >= maxSearchHits {
			return
		}
		if node.Type == html.ElementNode && node.Data == "div" && attribute(node, "data-type") == "web" {
			anchor := firstElement(node, func(candidate *html.Node) bool {
				return candidate.Data == "a" && strings.HasPrefix(attribute(candidate, "href"), "https://") && containsClass(candidate, "l1")
			})
			if anchor != nil {
				titleNode := firstElement(anchor, func(candidate *html.Node) bool { return containsClass(candidate, "search-snippet-title") })
				snippetNode := firstElement(node, func(candidate *html.Node) bool { return containsClass(candidate, "generic-snippet") })
				_, snippet := pageText(snippetNode)
				hit := agent.ResearchHit{URL: attribute(anchor, "href"), Title: normalizeSpace(nodeText(titleNode)), Snippet: truncateBytes(snippet, 800)}
				if hit.Title != "" && validateHitURL(hit.URL) {
					hits = append(hits, hit)
				}
			}
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(root)
	return hits
}

func validateHitURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && validatePublicURL(u) == nil
}

func pageText(root *html.Node) (string, string) {
	var title string
	var words []string
	var visit func(*html.Node, bool)
	visit = func(node *html.Node, hidden bool) {
		if node.Type == html.ElementNode {
			switch node.Data {
			case "script", "style", "noscript", "svg", "canvas", "template":
				hidden = true
			case "title":
				if title == "" {
					title = normalizeSpace(nodeText(node))
				}
			}
		}
		if node.Type == html.TextNode && !hidden {
			if value := normalizeSpace(node.Data); value != "" {
				words = append(words, value)
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child, hidden)
		}
	}
	visit(root, false)
	return title, normalizeSpace(strings.Join(words, " "))
}

func firstElement(root *html.Node, predicate func(*html.Node) bool) *html.Node {
	if root == nil {
		return nil
	}
	if root.Type == html.ElementNode && predicate(root) {
		return root
	}
	for child := root.FirstChild; child != nil; child = child.NextSibling {
		if found := firstElement(child, predicate); found != nil {
			return found
		}
	}
	return nil
}

func containsClass(node *html.Node, value string) bool {
	for _, class := range strings.Fields(attribute(node, "class")) {
		if class == value {
			return true
		}
	}
	return false
}

func attribute(node *html.Node, key string) string {
	if node == nil {
		return ""
	}
	for _, attribute := range node.Attr {
		if attribute.Key == key {
			return attribute.Val
		}
	}
	return ""
}

func nodeText(node *html.Node) string {
	if node == nil {
		return ""
	}
	var value strings.Builder
	var visit func(*html.Node)
	visit = func(current *html.Node) {
		if current.Type == html.TextNode {
			value.WriteString(current.Data)
			value.WriteByte(' ')
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(node)
	return value.String()
}

func normalizeSpace(value string) string {
	return strings.Join(strings.FieldsFunc(value, unicode.IsSpace), " ")
}

func truncateBytes(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for limit > 0 && (value[limit]&0xc0) == 0x80 {
		limit--
	}
	return strings.TrimSpace(value[:limit]) + " …"
}

func requestHash(request agent.ResearchRequest) string {
	data, _ := json.Marshal(request)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func resultHost(result agent.ResearchResult) string {
	if result.URL == "" {
		return ""
	}
	u, _ := url.Parse(result.URL)
	return u.Hostname()
}

// auditError reduces detailed fetch failures to non-sensitive categories. Raw
// parser and transport errors can contain a full URL, query, or credential and
// therefore must never be copied into the durable audit log.
func auditError(value string) string {
	lower := strings.ToLower(value)
	for _, category := range []struct {
		contains string
		name     string
	}{
		{"audit", "audit_unavailable"},
		{"parse url", "invalid_input"},
		{"credential", "url_credentials"},
		{"hostname", "invalid_hostname"},
		{"private or special", "blocked_destination"},
		{"requires an https url", "https_required"},
		{"port 443", "port_blocked"},
		{"redirect", "redirect_blocked"},
		{"content type", "content_type_blocked"},
		{"exceeds", "size_limit"},
		{"http ", "http_error"},
		{"timeout", "timeout"},
		{"deadline", "timeout"},
		{"resolve", "dns_error"},
		{"parse", "invalid_input"},
		{"search query is required", "invalid_query"},
		{"search query exceeds", "invalid_query"},
		{"research type must be", "invalid_type"},
		{"fetch", "fetch_error"},
	} {
		if strings.Contains(lower, category.contains) {
			return category.name
		}
	}
	return "request_failed"
}

type auditRecord struct {
	At          time.Time        `json:"at"`
	Participant chat.Participant `json:"participant"`
	RoomID      string           `json:"room_id"`
	Type        string           `json:"type"`
	InputHash   string           `json:"input_sha256"`
	Host        string           `json:"host,omitempty"`
	Outcome     string           `json:"outcome"`
	Error       string           `json:"error,omitempty"`
}

type auditLog struct {
	path string
	mu   sync.Mutex
}

func (a *auditLog) append(record auditRecord) error {
	if a == nil || strings.TrimSpace(a.path) == "" {
		return fmt.Errorf("audit path is unavailable")
	}
	record.At = time.Now().UTC()
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	directory := filepath.Dir(a.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(a.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	return file.Sync()
}
