package copilot

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/github/copilot-sdk/go/rpc"
	"github.com/timhavens/mohuddle/internal/chat"
)

func workspaceClient(root string) *Client {
	return &Client{policy: accessPolicy{
		profile: chat.PermissionWorkspace, workspace: root,
		readRoots: []string{root}, writeRoots: []string{root},
	}}
}

func TestRestrictedShellCannotEscapeThroughInterpreters(t *testing.T) {
	root := t.TempDir()
	client := workspaceClient(root)
	for _, command := range []string{
		"python3 ./task.py",
		`python3 -c 'import socket; socket.create_connection(("127.0.0.1", 9))'`,
		"node ./task.js", "sh ./task.sh", "powershell -File task.ps1",
		"go test ./...", "pwd", "",
	} {
		t.Run(command, func(t *testing.T) {
			request := rpc.PermissionRequestShell{FullCommandText: command, PossiblePaths: []string{root}}
			assertRejected(t, client, request)
			assertRejected(t, client, &request)
		})
	}
	client.policy.profile = chat.PermissionFull
	assertApproved(t, client, rpc.PermissionRequestShell{FullCommandText: "go test ./..."})
}

func TestRestrictedFileAccessRejectsLinksAndDirectoryTraversal(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	client := workspaceClient(root)
	if err := os.WriteFile(filepath.Join(outside, "dummy.txt"), []byte("audit fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "missing"), filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	paths := []string{
		filepath.Join(root, "link", "dummy.txt"),
		filepath.Join(root, "link", "new", "file.txt"),
		filepath.Join(root, "dangling"),
		"link/../dummy.txt",
		filepath.Join(outside, "dummy.txt"),
		root, // A recursive directory read must not grant its symlink descendants.
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			read := rpc.PermissionRequestRead{Path: path}
			write := rpc.PermissionRequestWrite{FileName: path}
			assertRejected(t, client, read)
			assertRejected(t, client, &read)
			assertRejected(t, client, write)
			assertRejected(t, client, &write)
		})
	}
	// Unrelated links do not prevent ordinary file access or new file creation.
	assertApproved(t, client, rpc.PermissionRequestWrite{FileName: filepath.Join(root, "new", "file.txt")})
}

func TestRestrictedFileAccessPreservesGrantedRootsAndDeniesBypass(t *testing.T) {
	root := t.TempDir()
	client := workspaceClient(root)
	path := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(path, []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	assertApproved(t, client, rpc.PermissionRequestRead{Path: "notes.txt"})
	assertApproved(t, client, rpc.PermissionRequestRead{Path: root})
	assertApproved(t, client, rpc.PermissionRequestWrite{FileName: path})
	assertApproved(t, client, rpc.PermissionRequestWrite{FileName: filepath.Join(root, "new", "notes.txt")})
	bypass := true
	read := rpc.PermissionRequestRead{Path: path, RequestSandboxBypass: &bypass}
	write := rpc.PermissionRequestWrite{FileName: path, RequestSandboxBypass: &bypass}
	assertRejected(t, client, read)
	assertRejected(t, client, &read)
	assertRejected(t, client, write)
	assertRejected(t, client, &write)
	assertRejected(t, client, rpc.PermissionRequestRead{Path: ""})
	client.policy.readRoots = []string{filepath.Join(root, "missing-root")}
	assertRejected(t, client, rpc.PermissionRequestRead{Path: filepath.Join(root, "missing-root", "file")})
}
