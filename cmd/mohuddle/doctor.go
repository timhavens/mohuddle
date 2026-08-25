package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/timhavens/mohuddle/internal/api"
	"github.com/timhavens/mohuddle/internal/chat"
	appsettings "github.com/timhavens/mohuddle/internal/settings"
	"github.com/timhavens/mohuddle/internal/speech"
	"github.com/timhavens/mohuddle/internal/store"
)

const doctorSchemaVersion = 1

const noProviderGuidance = "No operational provider CLI is available. Install and authenticate Codex, Claude, AGY, or Copilot, then run `mohuddle doctor` for detailed setup diagnostics."

type doctorOptions struct {
	json          bool
	stateDir      string
	configPath    string
	binaries      map[chat.Participant]string
	explicitPaths map[chat.Participant]bool
}

type doctorReport struct {
	SchemaVersion        int              `json:"schema_version"`
	MoHuddle             doctorProgram    `json:"mohuddle"`
	Paths                doctorPaths      `json:"paths"`
	Providers            []doctorProvider `json:"providers"`
	OperationalProviders int              `json:"operational_providers"`
	Speech               doctorSpeech     `json:"speech"`
	Platform             doctorPlatform   `json:"platform"`
}

type doctorProgram struct {
	Version string `json:"version"`
	Binary  string `json:"binary"`
}

type doctorPaths struct {
	Settings           string `json:"settings"`
	SettingsStatus     string `json:"settings_status"`
	SettingsError      string `json:"settings_error,omitempty"`
	RoomStateDirectory string `json:"room_state_directory"`
}

type doctorProvider struct {
	Name                 chat.Participant `json:"name"`
	ConfiguredPath       string           `json:"configured_path"`
	ExplicitPath         bool             `json:"explicit_path"`
	ResolvedPath         string           `json:"resolved_path,omitempty"`
	Status               string           `json:"status"`
	Detail               string           `json:"detail,omitempty"`
	Version              string           `json:"version,omitempty"`
	VersionError         string           `json:"version_error,omitempty"`
	Authentication       string           `json:"authentication"`
	AuthenticationDetail string           `json:"authentication_detail,omitempty"`
	Operational          bool             `json:"operational"`
	InstallURL           string           `json:"install_url"`
}

type doctorSpeech struct {
	Enabled      bool                     `json:"enabled"`
	Provider     string                   `json:"provider"`
	Status       string                   `json:"status"`
	Detail       string                   `json:"detail,omitempty"`
	Dependencies []doctorSpeechDependency `json:"dependencies"`
}

type doctorSpeechDependency struct {
	Name           string `json:"name"`
	ConfiguredPath string `json:"configured_path,omitempty"`
	ResolvedPath   string `json:"resolved_path,omitempty"`
	Status         string `json:"status"`
	Detail         string `json:"detail,omitempty"`
}

type doctorPlatform struct {
	OS           string          `json:"os"`
	Architecture string          `json:"architecture"`
	SupportTier  string          `json:"support_tier"`
	Features     []doctorFeature `json:"features"`
}

type doctorFeature struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Detail    string `json:"detail"`
}

func runDoctorCommand(args []string, output io.Writer) error {
	opts, err := parseDoctorOptions(args, output)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	report := collectDoctorReport(opts)
	if opts.json {
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	writeDoctorReport(output, report)
	return nil
}

func parseDoctorOptions(args []string, output io.Writer) (doctorOptions, error) {
	codexBinary := "codex"
	claudeBinary := "claude"
	agyBinary := "agy"
	copilotBinary := "copilot"
	value := doctorOptions{
		binaries:      make(map[chat.Participant]string),
		explicitPaths: make(map[chat.Participant]bool),
	}
	flags := flag.NewFlagSet("mohuddle doctor", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.BoolVar(&value.json, "json", false, "emit stable machine-readable diagnostics")
	flags.StringVar(&value.stateDir, "state-dir", "", "room state directory")
	flags.StringVar(&value.configPath, "config", "", "personal settings file")
	flags.StringVar(&codexBinary, "codex-binary", codexBinary, "Codex CLI binary")
	flags.StringVar(&claudeBinary, "claude-binary", claudeBinary, "Claude Code CLI binary")
	flags.StringVar(&agyBinary, "agy-binary", agyBinary, "Google Antigravity CLI binary")
	flags.StringVar(&copilotBinary, "copilot-binary", copilotBinary, "GitHub Copilot CLI binary")
	if err := flags.Parse(args); err != nil {
		return value, err
	}
	if flags.NArg() != 0 {
		return value, fmt.Errorf("doctor does not accept positional arguments")
	}
	flags.Visit(func(item *flag.Flag) {
		for _, provider := range chat.Agents() {
			if item.Name == string(provider)+"-binary" {
				value.explicitPaths[provider] = true
			}
		}
	})
	value.binaries = map[chat.Participant]string{
		chat.Codex: codexBinary, chat.Claude: claudeBinary, chat.Agy: agyBinary, chat.Copilot: copilotBinary,
	}
	return value, nil
}

func collectDoctorReport(opts doctorOptions) doctorReport {
	settingsPath, settingsPathErr := diagnosticPath(opts.configPath, appsettings.DefaultPath)
	statePath, statePathErr := diagnosticPath(opts.stateDir, store.DefaultStateDir)
	binaryPath, binaryErr := os.Executable()
	if binaryErr == nil {
		binaryPath, binaryErr = filepath.Abs(binaryPath)
	}
	if binaryErr != nil {
		binaryPath = "unavailable: " + binaryErr.Error()
	}

	report := doctorReport{
		SchemaVersion: doctorSchemaVersion,
		MoHuddle:      doctorProgram{Version: version, Binary: binaryPath},
		Paths: doctorPaths{
			Settings: settingsPath, RoomStateDirectory: statePath,
			SettingsStatus: "ok",
		},
		Platform: doctorPlatform{
			OS: runtime.GOOS, Architecture: runtime.GOARCH, SupportTier: platformSupportTier(),
			Features: []doctorFeature{{
				Name: "local_api", Available: api.LocalTransportSupported(), Detail: localAPIDetail(),
			}},
		},
	}
	if settingsPathErr != nil {
		report.Paths.SettingsStatus = "error"
		report.Paths.SettingsError = settingsPathErr.Error()
	}
	if statePathErr != nil {
		report.Paths.RoomStateDirectory = "unavailable: " + statePathErr.Error()
	}

	var preferences *appsettings.Store
	if settingsPathErr == nil {
		var err error
		preferences, err = appsettings.Open(settingsPath)
		if err != nil {
			report.Paths.SettingsStatus = "error"
			report.Paths.SettingsError = err.Error()
		} else if _, err := os.Stat(settingsPath); errors.Is(err, os.ErrNotExist) {
			report.Paths.SettingsStatus = "not_created"
		} else if err != nil {
			report.Paths.SettingsStatus = "error"
			report.Paths.SettingsError = err.Error()
		}
	}

	for _, provider := range chat.Agents() {
		item := diagnoseProvider(provider, opts.binaries[provider], opts.explicitPaths[provider])
		report.Providers = append(report.Providers, item)
		if item.Operational {
			report.OperationalProviders++
		}
	}

	speechConfig := speech.Config{}.WithDefaults()
	if preferences != nil {
		speechConfig = preferences.SpeechSettings()
	}
	report.Speech = diagnoseSpeech(speechConfig)
	return report
}

func diagnosticPath(configured string, defaultPath func() (string, error)) (string, error) {
	value := strings.TrimSpace(configured)
	if value == "" {
		var err error
		value, err = defaultPath()
		if err != nil {
			return "", err
		}
	}
	return filepath.Abs(value)
}

func diagnoseProvider(provider chat.Participant, configured string, explicit bool) doctorProvider {
	item := doctorProvider{
		Name: provider, ConfiguredPath: configured, ExplicitPath: explicit,
		Status: "missing", Authentication: "unknown", InstallURL: providerInstallURL(provider),
	}
	path, err := exec.LookPath(configured)
	if err != nil {
		if explicit || strings.ContainsAny(configured, `/\`) {
			item.Status = "path_error"
		}
		item.Detail = err.Error()
		return item
	}
	item.ResolvedPath, err = filepath.Abs(path)
	if err != nil {
		item.Status = "path_error"
		item.Detail = err.Error()
		return item
	}
	item.Status = "found"
	item.Version, item.VersionError = executableVersion(item.ResolvedPath)
	item.Authentication, item.AuthenticationDetail = providerAuthentication(provider, item.ResolvedPath)
	item.Operational = item.Authentication != "not_authenticated"
	return item
}

func executableVersion(path string) (string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", "version check timed out"
	}
	text := firstOutputLine(output)
	if err != nil {
		if text != "" {
			return "", text
		}
		return "", err.Error()
	}
	return text, ""
}

func providerAuthentication(provider chat.Participant, path string) (string, string) {
	var args []string
	switch provider {
	case chat.Codex:
		args = []string{"login", "status"}
	case chat.Claude:
		args = []string{"auth", "status"}
	default:
		return "unknown", "not safely detectable by MoHuddle"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "unknown", "authentication check timed out"
	}
	if err != nil {
		detail := firstOutputLine(output)
		if detail == "" {
			detail = err.Error()
		}
		return "not_authenticated", detail
	}
	return "authenticated", ""
}

func firstOutputLine(output []byte) string {
	text := strings.TrimSpace(string(output))
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	text = strings.TrimSpace(text)
	if len(text) > 512 {
		text = text[:512] + "..."
	}
	return text
}

func diagnoseSpeech(config speech.Config) doctorSpeech {
	config = config.WithDefaults()
	diagnostic := speech.DiagnoseRuntime(config)
	result := doctorSpeech{Enabled: config.Enabled, Provider: string(config.Provider), Status: diagnostic.Status}
	for _, dependency := range diagnostic.Dependencies {
		result.Dependencies = append(result.Dependencies, doctorSpeechDependency{
			Name: dependency.Name, ConfiguredPath: dependency.ConfiguredPath, ResolvedPath: dependency.ResolvedPath,
			Status: dependency.Status, Detail: dependency.Detail,
		})
	}
	if diagnostic.Status != "ready" {
		result.Detail = diagnostic.UnavailableSummary()
	} else if !config.Enabled {
		result.Detail = "speech is installed but disabled"
	}
	return result
}

func platformSupportTier() string {
	if runtime.GOOS == "linux" {
		return "supported"
	}
	return "preview"
}

func localAPIDetail() string {
	if api.LocalTransportSupported() {
		return "available"
	}
	if runtime.GOOS == "windows" {
		return "unavailable: Windows named-pipe transport is not implemented"
	}
	return "unavailable on this platform"
}

func providerInstallURL(provider chat.Participant) string {
	switch provider {
	case chat.Codex:
		return "https://learn.chatgpt.com/docs/codex/cli"
	case chat.Claude:
		return "https://code.claude.com/docs/en/getting-started"
	case chat.Agy:
		return "https://www.agy.dev/docs/cli/getting-started/"
	case chat.Copilot:
		return "https://docs.github.com/en/copilot/how-tos/copilot-cli/install-copilot-cli"
	default:
		return ""
	}
}

func writeDoctorReport(output io.Writer, report doctorReport) {
	fmt.Fprintln(output, "MoHuddle doctor")
	fmt.Fprintf(output, "Version: %s\n", report.MoHuddle.Version)
	fmt.Fprintf(output, "Binary: %s\n", report.MoHuddle.Binary)
	fmt.Fprintf(output, "Settings: %s (%s)\n", report.Paths.Settings, report.Paths.SettingsStatus)
	if report.Paths.SettingsError != "" {
		fmt.Fprintf(output, "  Settings error: %s\n", report.Paths.SettingsError)
	}
	fmt.Fprintf(output, "Room state: %s\n", report.Paths.RoomStateDirectory)
	fmt.Fprintln(output, "Providers:")
	for _, provider := range report.Providers {
		fmt.Fprintf(output, "  %s: %s\n", provider.Name, provider.Status)
		fmt.Fprintf(output, "    configured: %s\n", provider.ConfiguredPath)
		if provider.ResolvedPath != "" {
			fmt.Fprintf(output, "    resolved: %s\n", provider.ResolvedPath)
		}
		if provider.Version != "" {
			fmt.Fprintf(output, "    version: %s\n", provider.Version)
		} else if provider.VersionError != "" {
			fmt.Fprintf(output, "    version: unavailable (%s)\n", provider.VersionError)
		}
		fmt.Fprintf(output, "    authentication: %s\n", provider.Authentication)
		if provider.AuthenticationDetail != "" {
			fmt.Fprintf(output, "    authentication detail: %s\n", provider.AuthenticationDetail)
		}
		if provider.Detail != "" {
			fmt.Fprintf(output, "    detail: %s\n", provider.Detail)
		}
		if !provider.Operational {
			fmt.Fprintf(output, "    install/authenticate: %s\n", provider.InstallURL)
		}
	}
	fmt.Fprintf(output, "Operational providers: %d\n", report.OperationalProviders)
	fmt.Fprintf(output, "Speech: %s, provider=%s, enabled=%t", report.Speech.Status, report.Speech.Provider, report.Speech.Enabled)
	if report.Speech.Detail != "" {
		fmt.Fprintf(output, " (%s)", report.Speech.Detail)
	}
	fmt.Fprintln(output)
	for _, dependency := range report.Speech.Dependencies {
		path := dependency.ResolvedPath
		if path == "" {
			path = dependency.ConfiguredPath
		}
		fmt.Fprintf(output, "  %s: %s", dependency.Name, dependency.Status)
		if path != "" {
			fmt.Fprintf(output, " (%s)", path)
		}
		if dependency.Detail != "" {
			fmt.Fprintf(output, " — %s", dependency.Detail)
		}
		fmt.Fprintln(output)
	}
	fmt.Fprintf(output, "Platform: %s/%s (%s)\n", report.Platform.OS, report.Platform.Architecture, report.Platform.SupportTier)
	for _, feature := range report.Platform.Features {
		fmt.Fprintf(output, "  %s: %s\n", feature.Name, feature.Detail)
	}
	if report.OperationalProviders == 0 {
		fmt.Fprintln(output, "No operational provider is available. Install and authenticate at least one provider CLI above, then run this command again.")
	}
}

func writeNoProviderGuidance(output io.Writer) {
	fmt.Fprintln(output, "MoHuddle: "+noProviderGuidance)
}
