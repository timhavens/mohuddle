package settings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/speech"
)

func TestWorkerCountsPersistMigrateAndReturnCopies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{\n  \"version\": 5\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range chat.Agents() {
		if got := store.WorkerCounts()[provider]; got != 0 {
			t.Fatalf("legacy %s count=%d want=0", provider, got)
		}
	}
	if err := store.SetWorkerCount(chat.Codex, 2); err != nil {
		t.Fatal(err)
	}
	if err := store.SetWorkerCount(chat.Claude, 1); err != nil {
		t.Fatal(err)
	}

	counts := store.WorkerCounts()
	counts[chat.Codex] = 99
	if got := store.WorkerCounts()[chat.Codex]; got != 2 {
		t.Fatalf("WorkerCounts leaked backing map: codex=%d", got)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	wantCounts := map[chat.Participant]int{chat.Codex: 2, chat.Claude: 1, chat.Agy: 0, chat.Copilot: 0}
	if got := reopened.WorkerCounts(); !reflect.DeepEqual(got, wantCounts) {
		t.Fatalf("reopened WorkerCounts()=%v want=%v", got, wantCounts)
	}
	wantParticipants := []chat.Participant{chat.Codex, chat.Claude, chat.Agy, chat.Copilot, "codex-1", "codex-2", "claude-1"}
	if got := WorkerParticipants(reopened.WorkerCounts()); !reflect.DeepEqual(got, wantParticipants) {
		t.Fatalf("WorkerParticipants()=%v want=%v", got, wantParticipants)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted Config
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Version != currentVersion || persisted.Workers[chat.Codex] != 2 || persisted.Workers[chat.Claude] != 1 {
		t.Fatalf("persisted config=%+v", persisted)
	}
}

func TestProviderConcurrencyDefaultsMigrateAndFollowWorkers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{\n  \"version\": 9,\n  \"workers\": {\"codex\": 1}\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.ProviderConcurrency(chat.Codex); got != 2 {
		t.Fatalf("codex default concurrency=%d want=2", got)
	}
	if got := store.ProviderConcurrency(chat.Claude); got != 1 {
		t.Fatalf("claude default concurrency=%d want=1", got)
	}
	if got := store.ProviderConcurrency("unknown"); got != 0 {
		t.Fatalf("unknown provider concurrency=%d want=0", got)
	}
	if got := store.ProviderConcurrencyOverrides(); len(got) != 0 {
		t.Fatalf("legacy concurrency overrides=%v want empty", got)
	}

	if err := store.SetWorkerCount(chat.Codex, 0); err != nil {
		t.Fatal(err)
	}
	if got := store.ProviderConcurrency(chat.Codex); got != 1 {
		t.Fatalf("codex concurrency after removing worker=%d want=1", got)
	}
	if err := store.SetWorkerCount(chat.Claude, MaxWorkersPerProvider); err != nil {
		t.Fatal(err)
	}
	if got := store.ProviderConcurrency(chat.Claude); got != 2 {
		t.Fatalf("claude default concurrency with workers=%d want=2", got)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted Config
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Version != currentVersion {
		t.Fatalf("persisted version=%d want=%d", persisted.Version, currentVersion)
	}
	if persisted.ProviderConcurrency != nil {
		t.Fatalf("migration synthesized overrides=%v", persisted.ProviderConcurrency)
	}
}

func TestProviderConcurrencyOverridesPersistCapAndReturnCopies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetWorkerCount(chat.Codex, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.SetProviderConcurrency(chat.Codex, MaxProviderConcurrency); err != nil {
		t.Fatal(err)
	}
	if got := store.ProviderConcurrency(chat.Codex); got != 2 {
		t.Fatalf("effective concurrency above configured identities=%d want=2", got)
	}

	overrides := store.ProviderConcurrencyOverrides()
	overrides[chat.Codex] = 1
	if got := store.ProviderConcurrencyOverrides()[chat.Codex]; got != MaxProviderConcurrency {
		t.Fatalf("ProviderConcurrencyOverrides leaked backing map: codex=%d", got)
	}
	if err := store.SetWorkerCount(chat.Codex, MaxWorkersPerProvider); err != nil {
		t.Fatal(err)
	}
	if got := store.ProviderConcurrency(chat.Codex); got != MaxProviderConcurrency {
		t.Fatalf("effective concurrency with all identities=%d want=%d", got, MaxProviderConcurrency)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.ProviderConcurrencyOverrides(); !reflect.DeepEqual(got, map[chat.Participant]int{chat.Codex: MaxProviderConcurrency}) {
		t.Fatalf("reopened overrides=%v", got)
	}
	if got := reopened.ProviderConcurrency(chat.Codex); got != MaxProviderConcurrency {
		t.Fatalf("reopened effective concurrency=%d want=%d", got, MaxProviderConcurrency)
	}

	if err := reopened.ClearProviderConcurrency(chat.Codex); err != nil {
		t.Fatal(err)
	}
	if got := reopened.ProviderConcurrencyOverrides(); len(got) != 0 {
		t.Fatalf("cleared overrides=%v want empty", got)
	}
	if got := reopened.ProviderConcurrency(chat.Codex); got != 2 {
		t.Fatalf("default concurrency after clear=%d want=2", got)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.ProviderConcurrencyOverrides(); len(got) != 0 {
		t.Fatalf("persisted cleared overrides=%v want empty", got)
	}
}

func TestProviderConcurrencyValidationAndFailedUpdatesDoNotMutate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetProviderConcurrency(chat.Codex, 2); err != nil {
		t.Fatal(err)
	}
	want := store.ProviderConcurrencyOverrides()
	for name, update := range map[string]struct {
		provider chat.Participant
		capacity int
	}{
		"zero":          {chat.Codex, 0},
		"above maximum": {chat.Claude, MaxProviderConcurrency + 1},
		"aux provider":  {"codex-1", 1},
		"unknown":       {"unknown", 1},
	} {
		t.Run(name, func(t *testing.T) {
			if err := store.SetProviderConcurrency(update.provider, update.capacity); err == nil {
				t.Fatal("SetProviderConcurrency() succeeded, want error")
			}
			if got := store.ProviderConcurrencyOverrides(); !reflect.DeepEqual(got, want) {
				t.Fatalf("failed update mutated overrides: got=%v want=%v", got, want)
			}
		})
	}
	if err := store.ClearProviderConcurrency("codex-1"); err == nil {
		t.Fatal("ClearProviderConcurrency() accepted auxiliary provider")
	}
	if got := store.ProviderConcurrencyOverrides(); !reflect.DeepEqual(got, want) {
		t.Fatalf("failed clear mutated overrides: got=%v want=%v", got, want)
	}
}

func TestOpenRejectsInvalidPersistedProviderConcurrency(t *testing.T) {
	for name, values := range map[string]string{
		"zero":          `{"codex":0}`,
		"above maximum": fmt.Sprintf(`{"codex":%d}`, MaxProviderConcurrency+1),
		"aux provider":  `{"codex-1":1}`,
		"unknown":       `{"unknown":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			data := fmt.Sprintf("{\"version\":%d,\"provider_concurrency\":%s}\n", currentVersion, values)
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path); err == nil {
				t.Fatal("Open() accepted invalid persisted provider concurrency")
			}
		})
	}
}

func TestProgressModeDefaultsValidatesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{\"version\":6}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.ProgressDisplayMode(); got != chat.ProgressCompact {
		t.Fatalf("migrated progress mode=%q", got)
	}
	if err := store.SetProgressDisplayMode(chat.ProgressDetailed); err != nil {
		t.Fatal(err)
	}
	if err := store.SetProgressDisplayMode("noisy"); err == nil {
		t.Fatal("invalid progress mode succeeded")
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.ProgressDisplayMode(); got != chat.ProgressDetailed {
		t.Fatalf("persisted progress mode=%q", got)
	}
}

func TestWebSearchDefaultsOffAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if store.WebSearchEnabled() {
		t.Fatal("web search must default off")
	}
	if err := store.SetWebSearchEnabled(true); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.WebSearchEnabled() {
		t.Fatal("web search setting did not persist")
	}
}

func TestConversationResponderLimitDefaultsValidatesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.ConversationResponderLimit(); got != DefaultConversationResponders {
		t.Fatalf("default conversation responders=%d want=%d", got, DefaultConversationResponders)
	}
	if err := store.SetConversationResponderLimit(0); err != nil {
		t.Fatal(err)
	}
	if got := store.ConversationResponderLimit(); got != 0 {
		t.Fatalf("zero conversation responders=%d", got)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.ConversationResponderLimit(); got != 0 {
		t.Fatalf("persisted conversation responders=%d", got)
	}
	for _, invalid := range []int{-1, MaxConversationResponders + 1} {
		if err := reopened.SetConversationResponderLimit(invalid); err == nil {
			t.Fatalf("accepted invalid conversation responder limit %d", invalid)
		}
		if got := reopened.ConversationResponderLimit(); got != 0 {
			t.Fatalf("invalid update mutated responder limit to %d", got)
		}
	}
}

func TestOpenRejectsInvalidConversationResponderLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := fmt.Sprintf("{\"version\":%d,\"conversation_responders\":9}\n", currentVersion)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("Open accepted invalid conversation responder limit")
	}
}

func TestWorkerCountValidationAndFailedUpdatesDoNotMutate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for provider, count := range map[chat.Participant]int{chat.Codex: 3, chat.Claude: 3, chat.Agy: 2} {
		if err := store.SetWorkerCount(provider, count); err != nil {
			t.Fatal(err)
		}
	}
	want := store.WorkerCounts()

	for name, update := range map[string]struct {
		provider chat.Participant
		count    int
	}{
		"aggregate cap": {chat.Copilot, 1},
		"provider cap":  {chat.Codex, MaxWorkersPerProvider + 1},
		"negative":      {chat.Claude, -1},
		"aux provider":  {"codex-1", 1},
		"unknown":       {"unknown", 1},
	} {
		t.Run(name, func(t *testing.T) {
			if err := store.SetWorkerCount(update.provider, update.count); err == nil {
				t.Fatal("SetWorkerCount() succeeded, want error")
			}
			if got := store.WorkerCounts(); !reflect.DeepEqual(got, want) {
				t.Fatalf("failed update mutated counts: got=%v want=%v", got, want)
			}
		})
	}

	if err := store.SetWorkerCount(chat.Agy, 0); err != nil {
		t.Fatal(err)
	}
	if got := store.WorkerCounts()[chat.Agy]; got != 0 {
		t.Fatalf("zero count did not remove workers: %d", got)
	}
	if err := ValidateWorkerCounts(map[chat.Participant]int{chat.Codex: 3, chat.Claude: 3, chat.Agy: 3}); err == nil {
		t.Fatal("ValidateWorkerCounts accepted aggregate above cap")
	}
	if err := ValidateWorkerCounts(map[chat.Participant]int{"codex-1": 1}); err == nil {
		t.Fatal("ValidateWorkerCounts accepted auxiliary as provider")
	}
}

func TestOpenRejectsInvalidPersistedWorkerCounts(t *testing.T) {
	for name, workers := range map[string]string{
		"invalid provider": `{"codex-1":1}`,
		"provider cap":     `{"codex":4}`,
		"aggregate cap":    `{"codex":3,"claude":3,"agy":3}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			data := fmt.Sprintf("{\"version\":%d,\"workers\":%s}\n", currentVersion, workers)
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path); err == nil {
				t.Fatal("Open() accepted invalid persisted worker counts")
			}
		})
	}
}

func TestPersonalDefaultsPersistAndRoomOverridesWin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Default(chat.Codex).Permissions; got != chat.PermissionWorkspace {
		t.Fatalf("built-in permissions=%q", got)
	}
	if got := store.Default(chat.Agy).Permissions; got != chat.PermissionReadOnly {
		t.Fatalf("built-in AGY permissions=%q", got)
	}
	if got := store.Default(chat.Copilot).Permissions; got != chat.PermissionReadOnly {
		t.Fatalf("built-in Copilot permissions=%q", got)
	}
	if err := store.SetDefault(chat.Agy, chat.AgentSettings{Model: "agy-model"}); err != nil {
		t.Fatal(err)
	}
	if got := store.Default(chat.Agy); got.Model != "agy-model" || got.Permissions != chat.PermissionReadOnly {
		t.Fatalf("partial AGY default=%+v", got)
	}
	wantDefault := chat.AgentSettings{Model: "gpt-example", Effort: "high", Permissions: chat.PermissionFull}
	if err := store.AcknowledgeFullAccess(); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDefault(chat.Codex, wantDefault); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Default(chat.Codex); got != wantDefault {
		t.Fatalf("default=%+v want=%+v", got, wantDefault)
	}
	if !reopened.FullAccessAcknowledged() {
		t.Fatal("full access acknowledgement was not persisted")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode=%o", info.Mode().Perm())
	}
	room := chat.NewRoom("room", t.TempDir(), 4, time.Now())
	room.Settings = map[chat.Participant]chat.AgentSettings{
		chat.Codex: {Model: "room-model", Permissions: chat.PermissionReadOnly},
	}
	if got := reopened.Effective(room, chat.Codex); got.Model != "room-model" || got.Permissions != chat.PermissionReadOnly {
		t.Fatalf("room override=%+v", got)
	}
}

func TestDefaultPathUsesXDGConfigHome(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	path, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "mohuddle", "config.json")
	if path != want {
		t.Fatalf("path=%q want=%q", path, want)
	}
}

func TestDetailsPreferencePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if store.DetailsVisible() {
		t.Fatal("details should be quiet by default")
	}
	if err := store.SetDetailsVisible(true); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.DetailsVisible() {
		t.Fatal("details preference was not persisted")
	}
}

func TestCompletionSoundPreferencePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if store.CompletionSoundEnabled() {
		t.Fatal("completion sound should be off by default")
	}
	if err := store.SetCompletionSoundEnabled(true); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.CompletionSoundEnabled() {
		t.Fatal("completion sound preference was not persisted")
	}
	if err := reopened.SetCompletionSoundEnabled(false); err != nil {
		t.Fatal(err)
	}
	reopened, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.CompletionSoundEnabled() {
		t.Fatal("disabled completion sound preference was not persisted")
	}
}

func TestCoreDefaultsPersistAndLegacyConfigUsesBuiltInPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{\n  \"version\": 3\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	builtIn := store.DefaultCorePolicy()
	if got := len(builtIn.Preferred); got != 2 || builtIn.Preferred[0] != chat.Codex || builtIn.Preferred[1] != chat.Claude || builtIn.Failover != chat.CoreFailoverAuto {
		t.Fatalf("legacy core policy=%+v", builtIn)
	}
	want := chat.CorePolicy{
		Preferred: []chat.Participant{chat.Agy, chat.Copilot},
		Fallbacks: []chat.Participant{chat.Codex, chat.Claude},
		Failover:  chat.CoreFailoverPrompt, Restore: chat.CoreRestoreManual,
	}
	if err := store.SetDefaultCorePolicy(want); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.DefaultCorePolicy()
	if fmt.Sprint(got.Preferred) != fmt.Sprint(want.Preferred) || fmt.Sprint(got.Fallbacks) != fmt.Sprint(want.Fallbacks) || got.Failover != want.Failover || got.Restore != want.Restore {
		t.Fatalf("persisted core policy=%+v want=%+v", got, want)
	}
	got.Preferred[0] = chat.Codex
	if reopened.DefaultCorePolicy().Preferred[0] != chat.Agy {
		t.Fatal("default core policy leaked its backing slice")
	}
}

func TestSpeechSettingsPersistAndLegacyConfigMigrates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{\n  \"version\": 1\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	initial := store.SpeechSettings()
	if initial.Enabled || initial.Provider != speech.ProviderEdge || initial.Mode != speech.ModeAll ||
		initial.MaxChunkChars != speech.DefaultChunkChars || initial.MaxSegmentChars != speech.DefaultSegmentChars ||
		initial.WorkerNice == nil || *initial.WorkerNice != speech.DefaultWorkerNice || len(initial.Voices) != 0 {
		t.Fatalf("legacy speech defaults=%+v", initial)
	}
	want := speech.Config{
		Enabled: true, Provider: speech.ProviderKokoro, Mode: speech.ModeAgent, Agent: chat.Codex,
		Voices:    map[chat.Participant]string{chat.Codex: "am_adam"},
		ModelPath: "/tmp/example-model.onnx", MaxChunkChars: 2500, MaxSegmentChars: 180,
	}
	if err := store.SetSpeechSettings(want); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.SpeechSettings()
	if !got.Enabled || got.Provider != speech.ProviderKokoro || got.Mode != speech.ModeAgent || got.Agent != chat.Codex ||
		got.Voices[chat.Codex] != want.Voices[chat.Codex] || got.ModelPath != want.ModelPath ||
		got.MaxChunkChars != 2500 || got.MaxSegmentChars != 180 {
		t.Fatalf("speech=%+v", got)
	}
}
