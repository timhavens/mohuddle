package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const credentialsFile = "api_credentials.json"

type Credential struct {
	ID         string     `json:"id"`
	Token      string     `json:"token"`
	Kind       ClientKind `json:"kind"`
	InstanceID string     `json:"instance_id,omitempty"`
	Scopes     []Scope    `json:"scopes"`
}

type Credentials struct {
	InstanceID string       `json:"instance_id"`
	Entries    []Credential `json:"credentials"`
}

func CredentialsPath(stateRoot string) string {
	return filepath.Join(stateRoot, credentialsFile)
}

func LoadOrCreateCredentials(path string) (*Credentials, error) {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("API credentials path must be a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err == nil {
		var value Credentials
		if err := json.Unmarshal(data, &value); err != nil {
			return nil, fmt.Errorf("decode API credentials: %w", err)
		}
		if err := value.Validate(); err != nil {
			return nil, err
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, err
		}
		return &value, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	instanceID, err := NewID()
	if err != nil {
		return nil, err
	}
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	value := Credentials{
		InstanceID: "mohuddle-" + instanceID,
		Entries: []Credential{{
			ID: "local-admin", Token: token, Kind: ClientLocal,
			Scopes: []Scope{ScopeObserve, ScopeParticipate, ScopeAdminister},
		}},
	}
	if err := createPrivateJSON(path, value); errors.Is(err, os.ErrExist) {
		return LoadOrCreateCredentials(path)
	} else if err != nil {
		return nil, err
	}
	return &value, nil
}

func (c Credentials) Validate() error {
	if !validIdentifier(c.InstanceID) {
		return fmt.Errorf("invalid API instance identity")
	}
	if len(c.Entries) == 0 {
		return fmt.Errorf("at least one API credential is required")
	}
	seenIDs := make(map[string]bool, len(c.Entries))
	seenTokens := make(map[string]bool, len(c.Entries))
	for _, entry := range c.Entries {
		if !validIdentifier(entry.ID) || !entry.Kind.Valid() || len(entry.Token) < 32 || seenIDs[entry.ID] || seenTokens[entry.Token] {
			return fmt.Errorf("invalid or duplicate API credential %q", entry.ID)
		}
		seenIDs[entry.ID] = true
		seenTokens[entry.Token] = true
		if entry.Kind == ClientPeer && !validIdentifier(entry.InstanceID) {
			return fmt.Errorf("peer API credential %q requires a valid instance identity", entry.ID)
		}
		if len(entry.Scopes) == 0 {
			return fmt.Errorf("API credential %q has no scopes", entry.ID)
		}
		seenScopes := make(map[Scope]bool, len(entry.Scopes))
		for _, scope := range entry.Scopes {
			if !scope.Valid() || seenScopes[scope] {
				return fmt.Errorf("invalid or duplicate scope for API credential %q", entry.ID)
			}
			seenScopes[scope] = true
		}
	}
	return nil
}

func (c Credentials) Authenticate(token string) (Credential, bool) {
	for _, entry := range c.Entries {
		if subtle.ConstantTimeCompare([]byte(entry.Token), []byte(token)) == 1 {
			entry.Scopes = append([]Scope(nil), entry.Scopes...)
			sort.Slice(entry.Scopes, func(i, j int) bool { return entry.Scopes[i] < entry.Scopes[j] })
			return entry, true
		}
	}
	return Credential{}, false
}

func randomToken() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func createPrivateJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	file, err := os.CreateTemp(filepath.Dir(path), ".api-credentials-*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	// Linking a fully synced private file publishes it atomically without
	// replacing a credential set created by a concurrent process.
	if err := os.Link(temporary, path); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	return nil
}

func namespacedIdentity(instanceID, credentialID, clientID string) (string, error) {
	clientID = strings.TrimSpace(clientID)
	if !validIdentifier(clientID) {
		return "", fmt.Errorf("invalid client identity")
	}
	return instanceID + "/" + credentialID + "/" + clientID, nil
}
