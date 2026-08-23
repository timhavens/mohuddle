package remoteui

import (
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
)

var requiredAssets = []string{
	"index.html",
	"styles.css",
	"app.js",
	"api.js",
	"identity.js",
	"storage.js",
	"sw.js",
	"manifest.webmanifest",
	"icon.svg",
	"maskable.svg",
}

func asset(t *testing.T, name string) string {
	t.Helper()
	data, err := fs.ReadFile(FS(), name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if len(data) == 0 {
		t.Fatalf("%s is empty", name)
	}
	return string(data)
}

func TestEmbeddedApplicationContainsCompleteShell(t *testing.T) {
	for _, name := range requiredAssets {
		asset(t, name)
	}
	if _, err := fs.ReadFile(FS(), "../go.mod"); err == nil {
		t.Fatal("embedded filesystem allowed path traversal")
	}
}

func TestHTMLIsStrictCSPCompatible(t *testing.T) {
	html := asset(t, "index.html")
	required := []string{
		`Content-Security-Policy`,
		`script-src 'self'`,
		`style-src 'self'`,
		`connect-src 'self'`,
		`object-src 'none'`,
		`frame-ancestors 'none'`,
		`<script type="module" src="/app.js"></script>`,
		`rel="manifest" href="/manifest.webmanifest"`,
	}
	for _, value := range required {
		if !strings.Contains(html, value) {
			t.Errorf("index.html missing %q", value)
		}
	}
	for _, forbidden := range []string{"<style", " style=", "onclick=", "onload=", "javascript:", "'unsafe-inline'", "'unsafe-eval'", "https://", "http://"} {
		if strings.Contains(strings.ToLower(html), strings.ToLower(forbidden)) {
			t.Errorf("index.html contains forbidden inline or remote content %q", forbidden)
		}
	}
}

func TestManifestIsInstallableAndSelfContained(t *testing.T) {
	var manifest struct {
		Name     string `json:"name"`
		StartURL string `json:"start_url"`
		Scope    string `json:"scope"`
		Display  string `json:"display"`
		Icons    []struct {
			Source  string `json:"src"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	if err := json.Unmarshal([]byte(asset(t, "manifest.webmanifest")), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Name == "" || manifest.StartURL != "/" || manifest.Scope != "/" || manifest.Display != "standalone" {
		t.Fatalf("manifest is not an installable root-scoped application: %+v", manifest)
	}
	if len(manifest.Icons) < 2 {
		t.Fatalf("manifest icons=%+v", manifest.Icons)
	}
	for _, icon := range manifest.Icons {
		if !strings.HasPrefix(icon.Source, "/") || strings.Contains(icon.Source, "://") {
			t.Fatalf("manifest icon is not a local asset: %+v", icon)
		}
		asset(t, strings.TrimPrefix(icon.Source, "/"))
	}
}

func TestServiceWorkerCachesOnlyExplicitShell(t *testing.T) {
	worker := asset(t, "sw.js")
	for _, value := range []string{
		`event.request.method !== "GET"`,
		`url.pathname.startsWith("/api/")`,
		`!SHELL_PATHS.has(url.pathname)`,
	} {
		if !strings.Contains(worker, value) {
			t.Errorf("service worker missing cache guard %q", value)
		}
	}
	for _, forbidden := range []string{"/api/v1/", "indexedDB", "localStorage", "backgroundsync", "sync.register", "pushManager", "cookie", "draft", "transcript"} {
		if strings.Contains(strings.ToLower(worker), strings.ToLower(forbidden)) {
			t.Errorf("service worker contains forbidden dynamic-data capability %q", forbidden)
		}
	}
}

func TestClientUsesDeviceKeysAndFrozenGatewayContract(t *testing.T) {
	identity := asset(t, "identity.js")
	for _, value := range []string{
		`name: "ECDSA", namedCurve: "P-256"`,
		`crypto.subtle.generateKey`,
		`crypto.subtle.importKey`,
		`false,`,
		`hash: "SHA-256"`,
		`new TextEncoder().encode(String(payload))`,
	} {
		if !strings.Contains(identity, value) {
			t.Errorf("identity client missing %q", value)
		}
	}

	apiClient := asset(t, "api.js")
	for _, value := range []string{
		`/api/v1/pair`,
		`/api/v1/challenge`,
		`/api/v1/session`,
		`/api/v1/request`,
		`X-MoHuddle-CSRF`,
		`/api/v1/events`,
		`after_event`,
		`after_message`,
		`boot_id`,
		`credentials: "same-origin"`,
		`cache: "no-store"`,
	} {
		if !strings.Contains(apiClient, value) {
			t.Errorf("API client missing %q", value)
		}
	}
	if strings.Contains(apiClient, "route:") || strings.Contains(apiClient, "origin_instance_id") {
		t.Fatal("browser client must not construct authenticated route metadata")
	}
}

func TestClientImplementsScopedReconnectAndGapStates(t *testing.T) {
	app := asset(t, "app.js")
	for _, value := range []string{
		`case "sync"`,
		`case "event"`,
		`case "gap"`,
		`mergeHistory`,
		`message_sequence`,
		`event_sequence`,
		`challenge.payload`,
		`mode: "ask"`,
		`scopes().includes("participate")`,
		`markRevoked`,
		`event.code === 4001`,
		`frame.history.through`,
		`window.addEventListener("offline"`,
		`window.addEventListener("online"`,
	} {
		if !strings.Contains(app, value) {
			t.Errorf("application missing %q", value)
		}
	}
	for _, forbidden := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", ".style."} {
		if strings.Contains(app, forbidden) {
			t.Errorf("application contains unsafe DOM/code operation %q", forbidden)
		}
	}
}

func TestPersistedStateExcludesSessionsAndTranscripts(t *testing.T) {
	storage := asset(t, "storage.js")
	if !strings.Contains(storage, `const DEVICE_KEY = "device"`) || !strings.Contains(storage, "cursorKey") {
		t.Fatal("storage client omitted device or cursor state")
	}
	for _, forbidden := range []string{"transcript", "messages", "csrf", "session", "cookie", "draft"} {
		if strings.Contains(strings.ToLower(storage), forbidden) {
			t.Errorf("storage client persists forbidden state %q", forbidden)
		}
	}
}
