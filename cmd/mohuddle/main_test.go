package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/room"
	appsettings "github.com/timhavens/mohuddle/internal/settings"
	"github.com/timhavens/mohuddle/internal/store"
)

type restartRosterAgent struct{ participant chat.Participant }

func (a restartRosterAgent) Participant() chat.Participant { return a.participant }
func (a restartRosterAgent) Close() error                  { return nil }
func (a restartRosterAgent) Run(context.Context, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
	return agent.TurnResult{Done: true}, nil
}

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

func TestEffectiveSettingsAuxiliaryInheritsLaunchModelWithoutPermissionElevation(t *testing.T) {
	dir := t.TempDir()
	preferences, err := appsettings.Open(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	roomState := chat.NewRoom("room", dir, 3, time.Now())
	worker, _ := chat.AuxiliaryParticipant(chat.Codex, 1)
	launch := map[chat.Participant]chat.AgentSettings{
		chat.Codex: {Model: "gpt-worker", Effort: "high", Permissions: chat.PermissionFull},
	}
	got := effectiveSettings(preferences, roomState, launch, worker)
	if got.Model != "gpt-worker" || got.Effort != "high" || got.Permissions != chat.PermissionReadOnly {
		t.Fatalf("auxiliary settings=%+v", got)
	}

	roomState.Settings = map[chat.Participant]chat.AgentSettings{
		worker: {Permissions: chat.PermissionWorkspace},
	}
	got = effectiveSettings(preferences, roomState, launch, worker)
	if got.Permissions != chat.PermissionWorkspace {
		t.Fatalf("saved auxiliary permissions were replaced: %+v", got)
	}
}

func TestReconcileWorkerRosterPreservesDormantState(t *testing.T) {
	codexOne, _ := chat.AuxiliaryParticipant(chat.Codex, 1)
	codexTwo, _ := chat.AuxiliaryParticipant(chat.Codex, 2)
	codexThree, _ := chat.AuxiliaryParticipant(chat.Codex, 3)
	claudeOne, _ := chat.AuxiliaryParticipant(chat.Claude, 1)
	roomState := chat.NewRoom("room", t.TempDir(), 3, time.Now())
	roomState.Members[codexThree] = true
	roomState.Members[claudeOne] = true
	roomState.Sessions[codexOne] = chat.AgentSession{ID: "preserved-one", Cursor: 4}
	roomState.Sessions[codexThree] = chat.AgentSession{ID: "preserved-three", Cursor: 9}
	roomState.Settings = map[chat.Participant]chat.AgentSettings{
		codexThree: {Model: "preserved", Permissions: chat.PermissionWorkspace},
	}

	reconcileWorkerRoster(&roomState, map[chat.Participant]int{chat.Codex: 2})

	if roomState.Members[codexOne] {
		t.Fatal("explicitly absent configured worker was rejoined")
	}
	if _, known := roomState.Members[codexOne]; known {
		t.Fatal("legacy absent worker gained a membership entry")
	}
	if !roomState.Members[codexTwo] {
		t.Fatal("new configured worker was not joined")
	}
	if !roomState.Members[codexThree] || !roomState.Members[claudeOne] {
		t.Fatalf("deconfigured worker membership choices were not preserved: members=%v", roomState.Members)
	}
	if got := roomState.Sessions[codexOne]; got.ID != "preserved-one" || got.Cursor != 4 {
		t.Fatalf("configured worker session=%+v", got)
	}
	if got := roomState.Sessions[codexThree]; got.ID != "preserved-three" || got.Cursor != 9 {
		t.Fatalf("dormant worker session=%+v", got)
	}
	if got := roomState.Settings[codexThree]; got.Model != "preserved" || got.Permissions != chat.PermissionWorkspace {
		t.Fatalf("dormant worker settings=%+v", got)
	}
	if _, ok := roomState.Sessions[codexTwo]; !ok {
		t.Fatal("new worker session was not initialized")
	}

	reconcileWorkerRoster(&roomState, map[chat.Participant]int{chat.Codex: 3})
	if !roomState.Members[codexThree] {
		t.Fatal("restored worker did not rejoin")
	}
	if got := roomState.Sessions[codexThree]; got.ID != "preserved-three" || got.Cursor != 9 {
		t.Fatalf("restored worker session=%+v", got)
	}
}

func TestReconcileWorkerRosterPreservesHumanLeaveAcrossRestart(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	worker, _ := chat.AuxiliaryParticipant(chat.Codex, 1)
	roomState.Members[worker] = true
	roomState.Sessions[worker] = chat.AgentSession{ID: "worker-session", Cursor: 7}
	orchestrator, err := room.New(roomState, nil, roomStore,
		restartRosterAgent{participant: chat.Codex},
		restartRosterAgent{participant: worker},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.SetPresence(worker, false); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Close(); err != nil {
		t.Fatal(err)
	}

	loaded, err := roomStore.LoadRoom(roomState.ID)
	if err != nil {
		t.Fatal(err)
	}
	if present, known := loaded.Members[worker]; !known || present {
		t.Fatalf("human leave was not persisted explicitly: %v", loaded.Members)
	}
	reconcileWorkerRoster(&loaded, map[chat.Participant]int{chat.Codex: 1})
	if loaded.Present(worker) {
		t.Fatal("configured worker was rejoined after a human leave and runtime restart")
	}
	if got := loaded.Sessions[worker]; got.ID != "worker-session" || got.Cursor != 7 {
		t.Fatalf("worker session changed across restart: %+v", got)
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
	if _, err := parseOptions([]string{"--no-api", "--api-socket", filepath.Join(t.TempDir(), "api.sock")}); err == nil {
		t.Fatal("conflicting API flags were accepted")
	}
	value, err = parseOptions([]string{"--federation-listen", "127.0.0.1:4444"})
	if err != nil || value.federationListen != "127.0.0.1:4444" {
		t.Fatalf("federation listen=%q err=%v", value.federationListen, err)
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

func TestBuildAgentsCreatesConfiguredAuxiliaryIdentities(t *testing.T) {
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
	if err := preferences.SetWorkerCount(chat.Agy, 2); err != nil {
		t.Fatal(err)
	}
	agents, err := buildAgents(opts, roomState, preferences, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, instance := range agents {
			_ = instance.Close()
		}
	}()
	want := []chat.Participant{chat.Agy, "agy-1", "agy-2"}
	if len(agents) != len(want) {
		t.Fatalf("agents=%v", agents)
	}
	for index, instance := range agents {
		if got := instance.Participant(); got != want[index] {
			t.Fatalf("agent[%d]=%s want %s", index, got, want[index])
		}
	}
}

func TestBuildAgentsChecksProviderRuntimeOnceForAuxiliaries(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	fakeCodex := filepath.Join(dir, "codex")
	script := "#!/bin/sh\nprintf 'checked\\n' >> '" + calls + "'\nexit 0\n"
	if err := os.WriteFile(fakeCodex, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing")
	opts := options{
		codexBinary: fakeCodex, claudeBinary: missing, agyBinary: missing, copilotBinary: missing,
	}
	roomState := chat.NewRoom("room", dir, 4, time.Now())
	preferences, err := appsettings.Open(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := preferences.SetWorkerCount(chat.Codex, 2); err != nil {
		t.Fatal(err)
	}
	reconcileWorkerRoster(&roomState, preferences.WorkerCounts())
	agents, err := buildAgents(opts, roomState, preferences, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, instance := range agents {
			_ = instance.Close()
		}
	}()
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "checked\n"); got != 1 {
		t.Fatalf("authentication checks=%d, want 1", got)
	}
}

func TestBuildAgentsDoesNotAbortWhenPresentPreferredProviderIsMissing(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	opts := options{
		codexBinary: missing, claudeBinary: missing, agyBinary: missing, copilotBinary: missing,
	}
	roomState := chat.NewRoom("room", dir, 4, time.Now())
	preferences, err := appsettings.Open(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	agents, err := buildAgents(opts, roomState, preferences, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 0 {
		t.Fatalf("agents=%v", agents)
	}
}

func TestBuildAgentsRejectsExplicitMissingBinary(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "explicitly-missing")
	opts := options{
		codexBinary: missing, claudeBinary: "missing-claude", agyBinary: "missing-agy", copilotBinary: "missing-copilot",
		explicitBinaries: map[chat.Participant]bool{chat.Codex: true},
	}
	roomState := chat.NewRoom("room", dir, 4, time.Now())
	preferences, err := appsettings.Open(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildAgents(opts, roomState, preferences, nil); err == nil || !strings.Contains(err.Error(), "configured codex binary") {
		t.Fatalf("error=%v", err)
	}
}
