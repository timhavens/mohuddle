//go:build !windows

package tunnel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProfilePinsRoomWithoutEditingOriginalOrCopyingKeys(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MOHUDDLE_TEST_KEY", "secret-key-value")
	source := "control_plane:\n  base_url: https://api.openai.com\n  tunnel_id: tunnel_0123456789abcdef0123456789abcdef\n  api_key: env:MOHUDDLE_TEST_KEY\nmcp:\n  commands:\n    - channel: unwanted\n      command: unwanted-command\nhealth:\n  listen_addr: 0.0.0.0:8080\nlog:\n  http_raw_unsafe: true\n"
	path := filepath.Join(dir, "existing.yaml")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := readProfile(dir, "existing")
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.writeConfig(dir, "/path with 'quote/mohuddle", "/private/room ' one.json")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret-key-value") || strings.Contains(string(data), "unwanted-command") || strings.Contains(string(data), "0.0.0.0") {
		t.Fatal("unsafe settings or credentials copied")
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	commands := cfg["mcp"].(map[string]any)["commands"].([]any)
	if len(commands) != 1 || commands[0].(map[string]any)["channel"] != "main" {
		t.Fatal("extra MCP channels exposed")
	}
	command := commands[0].(map[string]any)["command"].(string)
	if command != "'/path with '\\''quote/mohuddle' chatgpt serve --connection '/private/room '\\'' one.json'" {
		t.Fatalf("unsafe command quoting: %s", command)
	}
	unchanged, _ := os.ReadFile(path)
	if string(unchanged) != source {
		t.Fatal("user's profile modified")
	}
	if info, _ := os.Stat(out); info.Mode().Perm() != 0o600 {
		t.Fatal("managed configuration is not private")
	}
}

func TestKeyReferencesAndProfileErrorsDoNotDiscloseSecrets(t *testing.T) {
	t.Setenv("MOHUDDLE_MISSING_KEY", "")
	for _, ref := range []string{"sk-secret-value", "env:INVALID-NAME", "env:MOHUDDLE_MISSING_KEY", "file:relative"} {
		err := validateKeyReference(ref)
		if err == nil || strings.Contains(err.Error(), "sk-secret-value") {
			t.Fatalf("unsafe reference accepted or leaked: %v", err)
		}
	}
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("secret-value"), 0o644); err != nil {
		t.Fatal(err)
	}
	if validateKeyReference("file:"+key) == nil {
		t.Fatal("world-readable key accepted")
	}
	if err := os.Chmod(key, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateKeyReference("file:" + key); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("control_plane: [sk-secret-value: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readProfile(dir, "bad"); err == nil || strings.Contains(err.Error(), "sk-secret-value") {
		t.Fatal("YAML parse error leaked profile source")
	}
}

func TestTunnelEnvironmentCannotOverrideRoomOrEnableUnsafeLogging(t *testing.T) {
	t.Setenv("MCP_COMMAND", "unwanted")
	t.Setenv("TUNNEL_CLIENT_CONFIG", "/wrong/room")
	t.Setenv("LOG_HTTP_RAW_UNSAFE", "true")
	t.Setenv("ALLOW_REMOTE_UI", "true")
	t.Setenv("CONTROL_PLANE_API_KEY", "keep-secret-in-env")
	t.Setenv("OPENAI_API_KEY", "unrelated-provider-key")
	values := tunnelEnvironment(map[string]any{"api_key": "env:CONTROL_PLANE_API_KEY"})
	joined := strings.Join(values, "\n")
	for _, prefix := range []string{"MCP_COMMAND=", "TUNNEL_CLIENT_CONFIG=", "LOG_HTTP_RAW_UNSAFE=", "ALLOW_REMOTE_UI=", "OPENAI_API_KEY="} {
		if strings.Contains(joined, prefix) {
			t.Fatalf("unsafe override inherited: %s", prefix)
		}
	}
	if !strings.Contains(joined, "CONTROL_PLANE_API_KEY=keep-secret-in-env") {
		t.Fatal("credential reference lost")
	}
}

func TestHealthURLMustBeLoopback(t *testing.T) {
	for _, value := range []string{"http://127.0.0.1:12", "http://[::1]:2345/"} {
		if !loopbackURL(value) {
			t.Fatal(value)
		}
	}
	for _, value := range []string{"https://example.com:12", "http://0.0.0.0:12", "http://user:pass@127.0.0.1:12", "http://127.0.0.1:12/?key=secret", "http://127.0.0.1:12/redirect"} {
		if loopbackURL(value) {
			t.Fatal(value)
		}
	}
}
