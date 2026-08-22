package settings

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
)

func TestPersonalDefaultsPersistAndRoomOverridesWin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Default(chat.Codex).Permissions; got != chat.PermissionWorkspace {
		t.Fatalf("built-in permissions=%q", got)
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
	if info.Mode().Perm() != 0o600 {
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
