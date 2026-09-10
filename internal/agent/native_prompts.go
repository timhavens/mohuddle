package agent

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/timhavens/mohuddle/internal/chat"
)

const (
	maxNativePromptFile    = 256 * 1024
	maxNativePromptTotal   = 1024 * 1024
	maxNativePromptSources = 64
)

type NativePromptSource struct {
	Path string
	Text string
	Note string
}

// NativePromptReport describes files available on disk, not a provider-confirmed
// dump of model context. No source is executed, modified, or sent to an AI.
type NativePromptReport struct {
	Sources []NativePromptSource
	Notes   []string
}

func InspectNativePrompts(participant chat.Participant, workspace string, model string) NativePromptReport {
	home, err := os.UserHomeDir()
	if err != nil {
		return NativePromptReport{Notes: []string{"Could not locate the user's configuration directory."}}
	}
	return inspectNativePrompts(participant.Provider(), workspace, home, os.Getenv, model)
}

type nativePromptReader struct {
	report          NativePromptReport
	seen            map[string]bool
	bytes           int
	model           string
	configuredModel string
}

func inspectNativePrompts(provider chat.Participant, workspace, home string, getenv func(string) string, models ...string) NativePromptReport {
	r := &nativePromptReader{seen: make(map[string]bool)}
	if len(models) > 0 {
		r.model = models[0]
	}
	r.report.Notes = []string{
		"Local instruction sources available on disk. Their presence does not confirm that a running provider loaded them.",
		"Built-in prompts are available only where a local model cache exposes them. Remote instructions, session memory, and dynamic skill/tool instructions are not included. Imports are shown as written; conditional rules may not apply to the current turn.",
	}
	configDir := func(variable, fallback string) string {
		if value := strings.TrimSpace(getenv(variable)); value != "" {
			return value
		}
		return filepath.Join(home, fallback)
	}
	dirs := nativePromptAncestors(workspace, provider != chat.Claude)
	switch provider {
	case chat.Codex:
		root := configDir("CODEX_HOME", ".codex")
		r.firstFile(root, []string{"AGENTS.override.md", "AGENTS.md"}, "global instructions")
		fallbacks := r.codexConfig(filepath.Join(root, "config.toml"), home)
		for _, dir := range dirs {
			if names := r.codexConfig(filepath.Join(dir, ".codex", "config.toml"), home); names != nil {
				fallbacks = names
			}
			r.firstFile(dir, append([]string{"AGENTS.override.md", "AGENTS.md"}, fallbacks...), "workspace instructions; first nonempty file in this directory")
		}
		r.codexModelCache(filepath.Join(root, "models_cache.json"))
		r.report.Notes = append(r.report.Notes, "Codex: MoHuddle supplies developer instructions. A room override replaces baseInstructions for MoHuddle's thread; project instruction discovery and managed policy are still provider-controlled.")
	case chat.Claude:
		root := configDir("CLAUDE_CONFIG_DIR", ".claude")
		managed := "/etc/claude-code/CLAUDE.md"
		if runtime.GOOS == "darwin" {
			managed = "/Library/Application Support/ClaudeCode/CLAUDE.md"
		} else if runtime.GOOS == "windows" {
			managed = filepath.Join(getenv("ProgramFiles"), "ClaudeCode", "CLAUDE.md")
		}
		r.file(managed, "managed instructions")
		r.file(filepath.Join(root, "CLAUDE.md"), "user instructions")
		r.rules(filepath.Join(root, "rules"), ".md")
		r.claudeSettings(filepath.Join(root, "settings.json"), root)
		for _, dir := range dirs {
			for _, name := range []string{"CLAUDE.md", "CLAUDE.local.md", ".claude/CLAUDE.md"} {
				r.file(filepath.Join(dir, name), "project instructions")
			}
			r.rules(filepath.Join(dir, ".claude", "rules"), ".md")
			r.claudeSettings(filepath.Join(dir, ".claude", "settings.json"), root)
			r.claudeSettings(filepath.Join(dir, ".claude", "settings.local.json"), root)
		}
		r.report.Notes = append(r.report.Notes, "Claude: MoHuddle uses --system-prompt for a room override and appends its coordination protocol. CLAUDE.md and conditional rules are separate provider context.")
	case chat.Agy:
		r.file(filepath.Join(home, ".gemini", "GEMINI.md"), "global Antigravity rules")
		for _, dir := range dirs {
			r.file(filepath.Join(dir, "GEMINI.md"), "workspace instruction source")
			r.rules(filepath.Join(dir, ".agents", "rules"), ".md")
			r.rules(filepath.Join(dir, ".agent", "rules"), ".md")
		}
		r.report.Notes = append(r.report.Notes, "AGY: the current print transport has no base-prompt replacement flag. MoHuddle supplies custom guidance in turn input; it cannot replace AGY's hidden base prompt. Isolated turns do not use the room workspace.")
	case chat.Copilot:
		root := configDir("COPILOT_HOME", ".copilot")
		r.file(filepath.Join(root, "copilot-instructions.md"), "user instructions")
		r.rules(filepath.Join(root, "instructions"), ".instructions.md")
		for _, dir := range dirs {
			for _, name := range []string{".github/copilot-instructions.md", "AGENTS.md", "CLAUDE.md", ".claude/CLAUDE.md", "GEMINI.md"} {
				r.file(filepath.Join(dir, name), "repository instruction source")
			}
			r.rules(filepath.Join(dir, ".github", "instructions"), ".instructions.md")
		}
		for _, dir := range strings.Split(getenv("COPILOT_CUSTOM_INSTRUCTIONS_DIRS"), ",") {
			if dir = strings.TrimSpace(dir); dir != "" {
				r.file(filepath.Join(dir, "AGENTS.md"), "additional instructions directory")
				r.rules(dir, ".instructions.md")
			}
		}
		r.report.Notes = append(r.report.Notes, "Copilot: MoHuddle disables native config, skills, and custom-instruction discovery. These files are reference sources, not automatically loaded by MoHuddle. Room overrides use the SDK's replace mode.")
	default:
		r.report.Notes = append(r.report.Notes, "Native instruction discovery is unavailable for this provider.")
	}
	return r.report
}

// Walk only the launch directory's ancestors, not the entire repository.
// Codex and Copilot project discovery starts at the nearest repository root.
func nativePromptAncestors(workspace string, repoBoundary bool) []string {
	dir, err := filepath.Abs(workspace)
	if err != nil {
		return nil
	}
	var result []string
	for {
		result = append(result, dir)
		if _, err := os.Stat(filepath.Join(dir, ".git")); repoBoundary && err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			if repoBoundary {
				return result[:1]
			}
			break
		}
		dir = parent
	}
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	return result
}

func (r *nativePromptReader) read(path string, limits ...int) ([]byte, error) {
	limit := maxNativePromptFile
	if len(limits) > 0 {
		limit = limits[0]
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, int64(limit)+1))
}

func (r *nativePromptReader) add(path, text, note string) {
	if r.seen[path] {
		return
	}
	r.seen[path] = true
	if r.bytes >= maxNativePromptTotal || len(r.report.Sources) >= maxNativePromptSources {
		if len(r.report.Notes) == 2 {
			r.report.Notes = append(r.report.Notes, "Source limit reached; additional files are omitted.")
		}
		return
	}
	limit := min(maxNativePromptFile, maxNativePromptTotal-r.bytes)
	if len(text) > limit {
		text = strings.ToValidUTF8(text[:limit], "") + "\n[truncated at viewer size limit]"
	}
	r.bytes += len(text)
	r.report.Sources = append(r.report.Sources, NativePromptSource{Path: path, Text: text, Note: note})
}

func (r *nativePromptReader) file(path, note string) bool {
	data, err := r.read(path)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		r.add(path, "[could not read instruction file]", note)
		return true
	}
	if strings.TrimSpace(string(data)) == "" {
		return false
	}
	r.add(path, string(data), note)
	return true
}

func (r *nativePromptReader) firstFile(dir string, names []string, note string) {
	for _, name := range names {
		if r.file(filepath.Join(dir, name), note) {
			return
		}
	}
}

func (r *nativePromptReader) rules(dir, suffix string) {
	visited := 0
	_ = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		visited++
		if visited > 1024 || len(r.report.Sources) >= maxNativePromptSources || r.bytes >= maxNativePromptTotal {
			r.report.Notes = append(r.report.Notes, "Rule discovery size limit reached for "+dir)
			return fs.SkipAll
		}
		if err == nil && !entry.IsDir() && strings.HasSuffix(entry.Name(), suffix) {
			r.file(path, "rule file; applicability is provider-controlled")
		}
		return nil
	})
}

func (r *nativePromptReader) codexConfig(path, home string) []string {
	data, err := r.read(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	var config struct {
		Model      string   `toml:"model"`
		Developer  string   `toml:"developer_instructions"`
		ModelFile  string   `toml:"model_instructions_file"`
		LegacyFile string   `toml:"experimental_instructions_file"`
		Fallbacks  []string `toml:"project_doc_fallback_filenames"`
	}
	if err != nil || len(data) > maxNativePromptFile {
		r.add(path, "[could not inspect prompt settings: config unreadable or too large]", "prompt settings only")
		return nil
	}
	if _, err := toml.Decode(string(data), &config); err != nil {
		r.add(path, "[could not parse TOML prompt settings]", "prompt settings only")
		return nil
	}
	if config.Model != "" {
		r.configuredModel = config.Model
	}
	if config.Developer != "" {
		r.add(path+"#developer_instructions", config.Developer, "native developer instructions; MoHuddle supplies its own developer instructions")
	}
	modelFile := config.ModelFile
	if modelFile == "" {
		modelFile = config.LegacyFile
	}
	if modelFile != "" {
		if strings.HasPrefix(modelFile, "~/") {
			modelFile = filepath.Join(home, modelFile[2:])
		} else if !filepath.IsAbs(modelFile) {
			modelFile = filepath.Join(filepath.Dir(path), modelFile)
		}
		if !r.file(modelFile, "native base prompt referenced by "+path) {
			r.add(modelFile, "[configured base prompt file is missing or empty]", "referenced by "+path)
		}
	}
	return config.Fallbacks
}

func (r *nativePromptReader) codexModelCache(path string) {
	const cacheLimit = 8 * 1024 * 1024
	data, err := r.read(path, cacheLimit)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var cache struct {
		Models []struct {
			Slug     string `json:"slug"`
			Base     string `json:"base_instructions"`
			Messages struct {
				Persistent string          `json:"persistent_instructions"`
				Template   string          `json:"instructions_template"`
				Variables  json.RawMessage `json:"instructions_variables"`
			} `json:"model_messages"`
		} `json:"models"`
	}
	if err != nil || len(data) > cacheLimit || json.Unmarshal(data, &cache) != nil {
		r.report.Notes = append(r.report.Notes, "Could not inspect the local Codex model-instruction cache.")
		return
	}
	model := r.model
	if model == "" || model == "default" || model == "auto" {
		model = r.configuredModel
	}
	for _, entry := range cache.Models {
		if entry.Slug != model {
			continue
		}
		text := entry.Base
		if entry.Messages.Persistent != "" {
			text += "\n\nPERSISTENT INSTRUCTIONS\n" + entry.Messages.Persistent
		}
		if entry.Messages.Template != "" {
			text += "\n\nINSTRUCTION TEMPLATE\n" + entry.Messages.Template
		}
		if len(entry.Messages.Variables) > 0 && string(entry.Messages.Variables) != "null" {
			text += "\n\nTEMPLATE VARIABLES\n" + string(entry.Messages.Variables)
		}
		if strings.TrimSpace(text) != "" {
			r.add(path+"#"+entry.Slug, strings.TrimSpace(text), "cached native model instructions; templates are not a confirmed rendering of the running session")
			return
		}
	}
	r.report.Notes = append(r.report.Notes, "The selected Codex model's base prompt was not found in the local cache.")
}

func (r *nativePromptReader) claudeSettings(path, userDir string) {
	data, err := r.read(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var config struct {
		OutputStyle string `json:"outputStyle"`
		Agent       string `json:"agent"`
	}
	if err != nil || len(data) > maxNativePromptFile || json.Unmarshal(data, &config) != nil {
		r.add(path, "[could not inspect prompt-related settings]", "prompt settings only")
		return
	}
	for _, selection := range []struct{ name, dir string }{{config.OutputStyle, "output-styles"}, {config.Agent, "agents"}} {
		if selection.name == "" {
			continue
		}
		r.add(path+"#"+selection.dir, selection.name, "configured selection; activation is provider-controlled")
		if filepath.Base(selection.name) != selection.name {
			continue
		}
		for _, dir := range []string{userDir, filepath.Dir(path)} {
			r.file(filepath.Join(dir, selection.dir, selection.name+".md"), "selected by "+path)
		}
	}
}
