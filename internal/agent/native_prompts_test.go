package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timhavens/mohuddle/internal/chat"
)

func writeNativePromptFixture(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func nativeReportText(report NativePromptReport) string {
	var parts []string
	for _, source := range report.Sources {
		parts = append(parts, source.Path, source.Text, source.Note)
	}
	return strings.Join(parts, "\n")
}

func TestNativeCodexPromptSourcesReadOnlyAndPromptFieldsOnly(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "personal")
	workspace := filepath.Join(dir, "repo", "child")
	config := filepath.Join(home, "custom-codex", "config.toml")
	configText := "model = 'test-model'\ndeveloper_instructions = \"\"\"\nNative developer guidance.\nSecond line.\"\"\"\nmodel_instructions_file = 'base.md'\nsecret_token = 'never-display-this-value'\n"
	writeNativePromptFixture(t, config, configText)
	writeNativePromptFixture(t, filepath.Join(home, "custom-codex", "base.md"), "Native base prompt")
	writeNativePromptFixture(t, filepath.Join(home, "custom-codex", "models_cache.json"), `{"models":[{"slug":"other","base_instructions":"unselected-cache-entry"},{"slug":"test-model","model_messages":{"instructions_template":"Cached built-in template"}}]}`)
	writeNativePromptFixture(t, filepath.Join(home, "custom-codex", "AGENTS.md"), "Global coding rules")
	writeNativePromptFixture(t, filepath.Join(dir, "repo", ".git"), "gitdir: /unused")
	writeNativePromptFixture(t, filepath.Join(dir, "repo", "AGENTS.md"), "Repository rules")
	writeNativePromptFixture(t, filepath.Join(workspace, "AGENTS.override.md"), "Child override")
	writeNativePromptFixture(t, filepath.Join(workspace, "AGENTS.md"), "shadowed-child-default")
	report := inspectNativePrompts(chat.Codex, workspace, home, func(key string) string {
		if key == "CODEX_HOME" {
			return filepath.Dir(config)
		}
		return ""
	})
	text := nativeReportText(report)
	for _, want := range []string{"Native developer guidance.\nSecond line.", "Native base prompt", "Global coding rules", "Repository rules", "Child override", "Cached built-in template"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing native source %q in %s", want, text)
		}
	}
	for _, unwanted := range []string{"never-display-this-value", "secret_token", "shadowed-child-default", "unselected-cache-entry"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("unexpected native output %q", unwanted)
		}
	}
	after, err := os.ReadFile(config)
	if err != nil || string(after) != configText {
		t.Fatal("native configuration was changed")
	}
}

func TestNativeProviderInstructionSourcesAndDiscoveryNotes(t *testing.T) {
	for _, provider := range []chat.Participant{chat.Claude, chat.Agy, chat.Copilot} {
		t.Run(string(provider), func(t *testing.T) {
			dir := t.TempDir()
			home, workspace := filepath.Join(dir, "personal"), filepath.Join(dir, "repo")
			writeNativePromptFixture(t, filepath.Join(workspace, ".git"), "gitdir: /unused")
			switch provider {
			case chat.Claude:
				writeNativePromptFixture(t, filepath.Join(home, ".claude", "CLAUDE.md"), "personal instructions")
				writeNativePromptFixture(t, filepath.Join(workspace, "CLAUDE.md"), "project instructions")
				writeNativePromptFixture(t, filepath.Join(home, ".claude", "settings.json"), `{"outputStyle":"terse","env":{"API_TOKEN":"hidden-token"}}`)
				writeNativePromptFixture(t, filepath.Join(home, ".claude", "output-styles", "terse.md"), "terse output style")
			case chat.Agy:
				writeNativePromptFixture(t, filepath.Join(home, ".gemini", "GEMINI.md"), "personal instructions")
				writeNativePromptFixture(t, filepath.Join(workspace, ".agents", "rules", "project.md"), "project instructions")
			case chat.Copilot:
				writeNativePromptFixture(t, filepath.Join(home, ".copilot", "copilot-instructions.md"), "personal instructions")
				writeNativePromptFixture(t, filepath.Join(workspace, ".github", "instructions", "project.instructions.md"), "project instructions")
			}
			report := inspectNativePrompts(provider, workspace, home, func(string) string { return "" })
			text := nativeReportText(report)
			if !strings.Contains(text, "personal instructions") || !strings.Contains(text, "project instructions") || strings.Contains(text, "hidden-token") {
				t.Fatalf("invalid native view: %s", text)
			}
			if provider == chat.Copilot && !strings.Contains(strings.Join(report.Notes, "\n"), "not automatically loaded") {
				t.Fatal("disabled native discovery was misrepresented")
			}
			if provider == chat.Agy && !strings.Contains(strings.Join(report.Notes, "\n"), "cannot replace") {
				t.Fatal("AGY replacement limitation was hidden")
			}
		})
	}
}

func TestNativePromptSourcesBoundOversizedFilesAndConfigErrors(t *testing.T) {
	dir := t.TempDir()
	writeNativePromptFixture(t, filepath.Join(dir, ".codex", "AGENTS.md"), strings.Repeat("x", maxNativePromptFile+100))
	writeNativePromptFixture(t, filepath.Join(dir, ".codex", "config.toml"), "secret = 'do-not-dump-this'\ninvalid = [")
	report := inspectNativePrompts(chat.Codex, dir, dir, func(string) string { return "" })
	text := nativeReportText(report)
	if !strings.Contains(text, "truncated") || !strings.Contains(text, "could not parse") || strings.Contains(text, "do-not-dump-this") {
		t.Fatal("size or config error handling was incorrect")
	}
	if len(text) > maxNativePromptTotal {
		t.Fatal("native report exceeded its bounded size")
	}
}
