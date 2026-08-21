package main

import (
	"testing"

	"github.com/timhavens/mohuddle/internal/chat"
)

func TestLaunchSettingsAreIndependentAndValidated(t *testing.T) {
	values, err := launchSettings(options{
		codexModel: "gpt-test", codexEffort: "high", codexPermissions: "workspace",
		claudeModel: "opus", claudeEffort: "xhigh", claudePermissions: "full",
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
	if _, err := launchSettings(options{claudeEffort: "ultra"}); err == nil {
		t.Fatal("unsupported Claude effort was accepted")
	}
	if _, err := launchSettings(options{codexPermissions: "unknown"}); err == nil {
		t.Fatal("unknown permission profile was accepted")
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
