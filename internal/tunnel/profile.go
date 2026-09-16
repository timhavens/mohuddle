// Package tunnel supervises the outbound ChatGPT tunnel owned by a room host.
// It never creates remote tunnels, takes over foreign processes, or stores keys.
package tunnel

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const DefaultProfile = "mohuddle-chatgpt"

var profileName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
var tunnelID = regexp.MustCompile(`^tunnel_[a-z0-9]{32}$`)
var envName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func ValidProfile(name string) bool { return profileName.MatchString(name) }

func DefaultProfileDir() (string, error) {
	if dir := os.Getenv("TUNNEL_CLIENT_PROFILE_DIR"); dir != "" {
		return filepath.Abs(dir)
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Abs(filepath.Join(dir, "tunnel-client"))
}

type profile struct {
	ControlPlane map[string]any `yaml:"control_plane"`
	Health       struct {
		ListenAddress string `yaml:"listen_addr"`
		URLFile       string `yaml:"url_file"`
	} `yaml:"health"`
	id, path string
}

func readProfile(dir, name string) (profile, error) {
	var p profile
	if !ValidProfile(name) {
		return p, fmt.Errorf("invalid tunnel profile name")
	}
	p.path = filepath.Join(dir, name+".yaml")
	f, err := os.Open(p.path)
	if err != nil {
		return p, fmt.Errorf("tunnel profile %s is unavailable; configure it once using tunnel-client init, or select /chatgpt profile NAME", name)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 256*1024+1))
	if err != nil || len(data) > 256*1024 {
		return p, fmt.Errorf("cannot read tunnel profile")
	}
	// YAML errors can include source lines containing credentials. Never echo them.
	if err := yaml.Unmarshal(data, &p); err != nil {
		return p, fmt.Errorf("invalid tunnel profile YAML; check it with tunnel-client doctor")
	}
	p.id, _ = p.ControlPlane["tunnel_id"].(string)
	if !tunnelID.MatchString(p.id) {
		return p, fmt.Errorf("tunnel profile needs a valid tunnel_id; keep the existing account-side tunnel")
	}
	ref, _ := p.ControlPlane["api_key"].(string)
	if err := validateKeyReference(ref); err != nil {
		return p, err
	}
	base, _ := p.ControlPlane["base_url"].(string)
	if base != "" {
		u, err := url.Parse(base)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return p, fmt.Errorf("tunnel control plane must use HTTPS without credentials in its URL")
		}
	}
	return p, nil
}

func validateKeyReference(ref string) error {
	if name, ok := strings.CutPrefix(ref, "env:"); ok && envName.MatchString(name) {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			return fmt.Errorf("%s is not set in MoHuddle's environment; launch MoHuddle from a shell where it is exported, or use a private file: key reference in the tunnel profile", name)
		}
		return nil
	}
	if path, ok := strings.CutPrefix(ref, "file:"); ok && filepath.IsAbs(path) {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() == 0 {
			return fmt.Errorf("tunnel key file must be a readable, nonempty private file (mode 0600)")
		}
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("tunnel key file is not readable")
		}
		return file.Close()
	}
	return fmt.Errorf("tunnel profile api_key must reference env:VARIABLE or file:/absolute/private/path; do not put a key in a room command")
}

// A fresh private config retains the control-plane settings and credential
// references, but exposes only this room's stdio bridge on the main channel.
// The user's profile is never changed. JSON is also valid YAML.
func (p profile) writeConfig(dir, executable, connection string) (string, error) {
	if !filepath.IsAbs(executable) || !filepath.IsAbs(connection) {
		return "", fmt.Errorf("managed tunnel requires absolute local paths")
	}
	command := quoteCommandArg(executable) + " chatgpt serve --connection " + quoteCommandArg(connection)
	data, err := json.Marshal(map[string]any{
		"config_version": 1,
		"control_plane":  p.ControlPlane,
		"mcp":            map[string]any{"commands": []any{map[string]string{"channel": "main", "command": command}}},
		"health":         map[string]string{"listen_addr": "127.0.0.1:0", "url_file": filepath.Join(dir, "health.url")},
		"admin_ui":       map[string]any{"open_browser": false},
		"log":            map[string]any{"level": "warn", "format": "json"},
	})
	if err != nil {
		return "", fmt.Errorf("cannot encode managed tunnel configuration")
	}
	path := filepath.Join(dir, "profile.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("cannot write private tunnel configuration")
	}
	return path, nil
}

func quoteCommandArg(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

// Do not let unrelated tunnel environment overrides replace the pinned room,
// enable payload logging, or expose the health UI. Keep referenced credentials.
func tunnelEnvironment(controlPlane map[string]any) []string {
	refs := make(map[string]bool)
	var collect func(any)
	collect = func(value any) {
		switch v := value.(type) {
		case string:
			if name, ok := strings.CutPrefix(v, "env:"); ok && envName.MatchString(name) {
				refs[name] = true
			}
		case map[string]any:
			for _, item := range v {
				collect(item)
			}
		case []any:
			for _, item := range v {
				collect(item)
			}
		}
	}
	collect(controlPlane)
	var env []string
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		blocked := name == "OPENAI_API_KEY" || name == "PID_FILE" || name == "ALLOW_REMOTE_UI" || name == "OPEN_WEB_UI" || name == "ADMIN_UI_LOG_BUFFER_EVENTS"
		for _, prefix := range []string{"TUNNEL_CLIENT_", "CONTROL_PLANE_", "MCP_", "HARPOON_", "HEALTH_", "CLOUDFLARED_", "LOG_"} {
			blocked = blocked || strings.HasPrefix(name, prefix)
		}
		if !blocked || refs[name] {
			env = append(env, item)
		}
	}
	return env
}
