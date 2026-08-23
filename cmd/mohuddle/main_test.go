package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
	appsettings "github.com/timhavens/mohuddle/internal/settings"
)

func TestLaunchSettingsAreIndependentAndValidated(t *testing.T) {
	values, err := launchSettings(options{
		codexModel: "gpt-test", codexEffort: "high", codexPermissions: "workspace",
		claudeModel: "opus", claudeEffort: "xhigh", claudePermissions: "full",
		agyModel: "gemini-test", agyEffort: "medium", agyPermissions: "read-only",
		copilotModel: "copilot-test", copilotEffort: "minimal", copilotPermissions: "workspace",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := values[chat.Codex]; got.Model != "gpt-test" || got.Effort != "high" || got.Permissions != chat.PermissionWorkspace {
		t.Fatalf("Codex settings=%+v", got)
	}
	if got := values[chat.Claude]; got.Model != "opus" || got.Effort != "xhigh" || got.Permissions != chat.PermissionFull {
		t.Fatalf("Claude settings=%+v", got)
	}
	if got := values[chat.Agy]; got.Model != "gemini-test" || got.Effort != "medium" || got.Permissions != chat.PermissionReadOnly {
		t.Fatalf("AGY settings=%+v", got)
	}
	if got := values[chat.Copilot]; got.Model != "copilot-test" || got.Effort != "minimal" || got.Permissions != chat.PermissionWorkspace {
		t.Fatalf("Copilot settings=%+v", got)
	}
	if _, err := launchSettings(options{claudeEffort: "ultra"}); err == nil {
		t.Fatal("unsupported Claude effort was accepted")
	}
	if _, err := launchSettings(options{codexPermissions: "unknown"}); err == nil {
		t.Fatal("unknown permission profile was accepted")
	}
	if _, err := launchSettings(options{agyEffort: "xhigh"}); err == nil {
		t.Fatal("unsupported AGY effort was accepted")
	}
	if _, err := launchSettings(options{copilotEffort: "ultra"}); err == nil {
		t.Fatal("unsupported Copilot effort was accepted")
	}
}

func TestMergeSettingsOnlyOverridesExplicitFields(t *testing.T) {
	base := chat.AgentSettings{Model: "base", Effort: "medium", Permissions: chat.PermissionWorkspace}
	got := mergeSettings(base, chat.AgentSettings{Model: "launch"})
	want := chat.AgentSettings{Model: "launch", Effort: "medium", Permissions: chat.PermissionWorkspace}
	if got != want {
		t.Fatalf("merged=%+v want=%+v", got, want)
	}
}

func TestEffectiveSettingsKeepOptionalDefaultAndAcceptLaunchOverride(t *testing.T) {
	dir := t.TempDir()
	preferences, err := appsettings.Open(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	roomState := chat.NewRoom("room", dir, 3, time.Now())
	if got := effectiveSettings(preferences, roomState, nil, chat.Agy).Permissions; got != chat.PermissionReadOnly {
		t.Fatalf("AGY built-in permissions=%s", got)
	}
	launch := map[chat.Participant]chat.AgentSettings{
		chat.Agy: {Permissions: chat.PermissionWorkspace},
	}
	if got := effectiveSettings(preferences, roomState, launch, chat.Agy).Permissions; got != chat.PermissionWorkspace {
		t.Fatalf("AGY launch permissions=%s", got)
	}
}

func TestParseOptionsSupportsMaxWavesAndDeprecatedAlias(t *testing.T) {
	value, err := parseOptions([]string{"--max-waves", "5"})
	if err != nil || value.maxWaves != 5 {
		t.Fatalf("max waves=%d err=%v", value.maxWaves, err)
	}
	value, err = parseOptions([]string{"--max-turns", "4"})
	if err != nil || value.maxWaves != 4 {
		t.Fatalf("deprecated alias max waves=%d err=%v", value.maxWaves, err)
	}
	if _, err := parseOptions([]string{"--max-waves", "3", "--max-turns", "3"}); err == nil {
		t.Fatal("both cap flags were accepted")
	}
	if _, err := parseOptions([]string{"--max-waves", "0"}); err == nil {
		t.Fatal("zero max waves was accepted")
	}
}

func TestBuildAgentsIncludesOnlyInstalledOptionalProviders(t *testing.T) {
	dir := t.TempDir()
	fakeAGY := filepath.Join(dir, "agy")
	if err := os.WriteFile(fakeAGY, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing")
	opts := options{
		codexBinary: missing, claudeBinary: missing, agyBinary: fakeAGY, copilotBinary: missing,
	}
	roomState := chat.NewRoom("room", dir, 4, time.Now())
	roomState.Members = map[chat.Participant]bool{chat.Agy: true}
	preferences, err := appsettings.Open(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	agents, err := buildAgents(opts, roomState, preferences, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Participant() != chat.Agy {
		t.Fatalf("agents=%v", agents)
	}
}
