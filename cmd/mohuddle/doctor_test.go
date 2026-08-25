package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/timhavens/mohuddle/internal/chat"
)

func TestMain(m *testing.M) {
	base := strings.TrimSuffix(filepath.Base(os.Args[0]), filepath.Ext(os.Args[0]))
	if strings.HasPrefix(base, "doctor-helper-") {
		runDoctorHelperProcess(strings.TrimPrefix(base, "doctor-helper-"), os.Args[1:])
		return
	}
	os.Exit(m.Run())
}

func runDoctorHelperProcess(provider string, args []string) {
	if len(args) == 1 && args[0] == "--version" {
		fmtVersion := map[string]string{"codex": "codex-cli 1.2.3", "claude": "claude 4.5.6", "agy": "agy 7.8.9"}
		if value := fmtVersion[provider]; value != "" {
			_, _ = os.Stdout.WriteString(value + "\n")
			os.Exit(0)
		}
	}
	if provider == "codex" && strings.Join(args, " ") == "login status" {
		if calls := os.Getenv("MOHUDDLE_DOCTOR_HELPER_CALLS"); calls != "" {
			file, err := os.OpenFile(calls, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				os.Exit(2)
			}
			_, err = file.WriteString("checked\n")
			closeErr := file.Close()
			if err != nil || closeErr != nil {
				os.Exit(2)
			}
		}
		os.Exit(0)
	}
	if provider == "claude" && strings.Join(args, " ") == "auth status" {
		_, _ = os.Stdout.WriteString("not logged in\n")
		os.Exit(1)
	}
	os.Exit(1)
}

func TestDoctorReportDistinguishesProviderStates(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config-root"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state-root"))

	codexPath := writeDoctorHelperExecutable(t, dir, "codex")
	claudePath := writeDoctorHelperExecutable(t, dir, "claude")
	agyPath := writeDoctorHelperExecutable(t, dir, "agy")
	missing := filepath.Join(dir, "missing-copilot")
	report := collectDoctorReport(doctorOptions{
		configPath: filepath.Join(dir, "settings.json"),
		stateDir:   filepath.Join(dir, "rooms"),
		binaries: map[chat.Participant]string{
			chat.Codex: codexPath, chat.Claude: claudePath, chat.Agy: agyPath, chat.Copilot: missing,
		},
		explicitPaths: map[chat.Participant]bool{
			chat.Codex: true, chat.Claude: true, chat.Agy: true, chat.Copilot: true,
		},
	})
	if report.SchemaVersion != doctorSchemaVersion {
		t.Fatalf("schema version=%d", report.SchemaVersion)
	}
	if report.Paths.SettingsStatus != "not_created" {
		t.Fatalf("settings status=%q error=%q", report.Paths.SettingsStatus, report.Paths.SettingsError)
	}
	if report.OperationalProviders != 2 {
		t.Fatalf("operational providers=%d, providers=%+v", report.OperationalProviders, report.Providers)
	}

	codex := report.Providers[0]
	if codex.Status != "found" || codex.Version != "codex-cli 1.2.3" || codex.Authentication != "authenticated" || !codex.Operational {
		t.Fatalf("Codex diagnostic=%+v", codex)
	}
	claude := report.Providers[1]
	if claude.Status != "found" || claude.Authentication != "not_authenticated" || claude.Operational || !strings.Contains(claude.AuthenticationDetail, "not logged in") {
		t.Fatalf("Claude diagnostic=%+v", claude)
	}
	agy := report.Providers[2]
	if agy.Authentication != "unknown" || !agy.Operational || agy.Version != "agy 7.8.9" {
		t.Fatalf("AGY diagnostic=%+v", agy)
	}
	copilot := report.Providers[3]
	if copilot.Status != "path_error" || copilot.Operational || copilot.InstallURL == "" {
		t.Fatalf("Copilot diagnostic=%+v", copilot)
	}
}

func TestDoctorJSONIsStableAndDoesNotCreateState(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "rooms")
	configPath := filepath.Join(dir, "config.json")
	missing := filepath.Join(dir, "missing")
	var output bytes.Buffer
	err := runDoctorCommand([]string{
		"--json", "--state-dir", stateDir, "--config", configPath,
		"--codex-binary", missing, "--claude-binary", missing,
		"--agy-binary", missing, "--copilot-binary", missing,
	}, &output)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("doctor created state directory: %v", err)
	}
	var decoded doctorReport
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatalf("decode doctor JSON: %v\n%s", err, output.String())
	}
	if decoded.SchemaVersion != 1 || decoded.Paths.RoomStateDirectory != stateDir || decoded.Paths.Settings != configPath {
		t.Fatalf("doctor JSON=%+v", decoded)
	}
	if decoded.OperationalProviders != 0 || len(decoded.Providers) != 4 {
		t.Fatalf("provider summary=%+v", decoded)
	}
	for _, provider := range decoded.Providers {
		if provider.Status != "path_error" || provider.ConfiguredPath != missing {
			t.Fatalf("provider=%+v", provider)
		}
	}
}

func TestDoctorReportsMalformedSettingsWithoutFailing(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing")
	report := collectDoctorReport(doctorOptions{
		configPath: configPath,
		stateDir:   filepath.Join(dir, "state"),
		binaries: map[chat.Participant]string{
			chat.Codex: missing, chat.Claude: missing, chat.Agy: missing, chat.Copilot: missing,
		},
		explicitPaths: map[chat.Participant]bool{},
	})
	if report.Paths.SettingsStatus != "error" || !strings.Contains(report.Paths.SettingsError, "decode config") {
		t.Fatalf("settings diagnostic=%+v", report.Paths)
	}
}

func TestDoctorHumanOutputAndStartupGuidanceAreActionable(t *testing.T) {
	report := doctorReport{
		SchemaVersion: doctorSchemaVersion,
		MoHuddle:      doctorProgram{Version: "v1.2.3", Binary: "/bin/mohuddle"},
		Paths: doctorPaths{
			Settings: "/config.json", SettingsStatus: "not_created", RoomStateDirectory: "/state",
		},
		Providers: []doctorProvider{{
			Name: chat.Codex, ConfiguredPath: "codex", Status: "missing", Authentication: "unknown",
			InstallURL: providerInstallURL(chat.Codex),
		}},
		Speech: doctorSpeech{Provider: "kokoro", Status: "unavailable", Detail: "model missing"},
		Platform: doctorPlatform{OS: "linux", Architecture: "amd64", SupportTier: "supported", Features: []doctorFeature{{
			Name: "local_api", Available: true, Detail: "available",
		}}},
	}
	var output bytes.Buffer
	writeDoctorReport(&output, report)
	for _, want := range []string{"MoHuddle doctor", "Settings: /config.json", "codex: missing", "install/authenticate:", "No operational provider is available"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("human output missing %q:\n%s", want, output.String())
		}
	}
	output.Reset()
	writeNoProviderGuidance(&output)
	if !strings.Contains(output.String(), "mohuddle doctor") || !strings.Contains(output.String(), "authenticate") {
		t.Fatalf("startup guidance=%q", output.String())
	}
}

func TestParseDoctorOptionsRejectsUnexpectedInput(t *testing.T) {
	var output bytes.Buffer
	if _, err := parseDoctorOptions([]string{"unexpected"}, &output); err == nil {
		t.Fatal("doctor accepted a positional argument")
	}
	value, err := parseDoctorOptions([]string{"--json", "--codex-binary", "/custom/codex"}, &output)
	if err != nil {
		t.Fatal(err)
	}
	if !value.json || value.binaries[chat.Codex] != "/custom/codex" || !value.explicitPaths[chat.Codex] {
		t.Fatalf("doctor options=%+v", value)
	}
}

func TestRemotePhoneGatewayDiagnosticMatchesPlatform(t *testing.T) {
	detail := remotePhoneGatewayDetail()
	if runtime.GOOS == "windows" {
		if !strings.Contains(detail, "unavailable") || !strings.Contains(detail, "POSIX") {
			t.Fatalf("Windows remote gateway detail=%q", detail)
		}
		return
	}
	if detail != "available" {
		t.Fatalf("remote gateway detail=%q", detail)
	}
}

func writeDoctorHelperExecutable(t *testing.T, dir, provider string) string {
	t.Helper()
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	name := "doctor-helper-" + provider
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
