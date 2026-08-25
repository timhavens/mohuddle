package api

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestLoadOrCreateCredentialsSecuresAndReusesIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "credentials.json")
	first, err := LoadOrCreateCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	if len(first.Entries) != 1 || len(first.Entries[0].Token) < 32 {
		t.Fatalf("credentials=%+v", first)
	}
	second, err := LoadOrCreateCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if second.InstanceID != first.InstanceID || second.Entries[0].Token != first.Entries[0].Token {
		t.Fatalf("credentials changed across load: first=%+v second=%+v", first, second)
	}
}

func TestCredentialsRequirePeerInstanceIdentity(t *testing.T) {
	value := Credentials{InstanceID: "host", Entries: []Credential{{
		ID: "peer", Token: "01234567890123456789012345678901", Kind: ClientPeer, Scopes: []Scope{ScopeObserve},
	}}}
	if err := value.Validate(); err == nil {
		t.Fatal("peer credential without an instance identity was accepted")
	}
}

func TestClientIdentityCannotAlterNamespace(t *testing.T) {
	value := Credentials{InstanceID: "host", Entries: []Credential{{
		ID: "local", Token: "01234567890123456789012345678901", Kind: ClientLocal, Scopes: []Scope{ScopeObserve},
	}}}
	service, err := NewService(value, newFakeController())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(HelloRequest{ClientID: "../another/namespace", Token: value.Entries[0].Token}); err == nil {
		t.Fatal("namespace-altering client identity was accepted")
	}
}

func TestConcurrentCredentialCreationConverges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "credentials.json")
	values := make([]*Credentials, 12)
	errors := make([]error, len(values))
	var wait sync.WaitGroup
	for index := range values {
		wait.Add(1)
		go func() {
			defer wait.Done()
			values[index], errors[index] = LoadOrCreateCredentials(path)
		}()
	}
	wait.Wait()
	for index, err := range errors {
		if err != nil {
			t.Fatalf("load %d: %v", index, err)
		}
		if values[index].InstanceID != values[0].InstanceID || values[index].Entries[0].Token != values[0].Entries[0].Token {
			t.Fatalf("credential %d diverged", index)
		}
	}
}

func TestCredentialsRejectSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "credentials.json")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateCredentials(path); err == nil {
		t.Fatal("credential symlink was accepted")
	}
}
