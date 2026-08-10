package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const oauthClientsStateVersion = 1

type oauthClientsState struct {
	Version int                    `json:"version"`
	Issuer  string                 `json:"issuer"`
	Clients map[string]oauthClient `json:"clients"`
}

func NewPersistentOAuthProvider(publicBase, adminPIN, jwtSecret, clientsFile string) (*OAuthProvider, error) {
	p := NewOAuthProvider(publicBase, adminPIN, jwtSecret)
	p.clientsFile = clientsFile
	if err := p.loadClients(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *OAuthProvider) loadClients() error {
	if p.clientsFile == "" {
		return nil
	}

	state, err := readOAuthClientsState(p.clientsFile)
	if err != nil {
		return err
	}
	if state == nil {
		return nil
	}
	if state.Version != oauthClientsStateVersion {
		return fmt.Errorf("unsupported OAuth client state version %d", state.Version)
	}
	if state.Issuer != p.publicBase {
		return fmt.Errorf("OAuth client state issuer does not match ZSHELL_PUBLIC_BASE_URL")
	}
	if state.Clients == nil {
		state.Clients = make(map[string]oauthClient)
	}
	if len(state.Clients) > maxClients {
		return fmt.Errorf("OAuth client state contains too many clients")
	}

	for id, client := range state.Clients {
		if id == "" || client.ID != id || len(client.Name) > maxClientName {
			return fmt.Errorf("OAuth client state contains invalid client metadata")
		}
		if len(client.RedirectURIs) == 0 || len(client.RedirectURIs) > maxRedirectURIs {
			return fmt.Errorf("OAuth client %q has invalid redirect URI count", id)
		}
		for _, redirectURI := range client.RedirectURIs {
			if !validRedirectURI(redirectURI) {
				return fmt.Errorf("OAuth client %q has invalid redirect URI", id)
			}
		}
	}

	p.clients = state.Clients
	return nil
}

func readOAuthClientsState(path string) (*oauthClientsState, error) {
	candidates := []string{path, path + ".bak"}
	var failures []error
	for _, candidate := range candidates {
		data, err := os.ReadFile(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("read OAuth clients %s: %w", candidate, err))
			continue
		}
		var state oauthClientsState
		if err := json.Unmarshal(data, &state); err != nil {
			failures = append(failures, fmt.Errorf("decode OAuth clients %s: %w", candidate, err))
			continue
		}
		return &state, nil
	}
	if len(failures) != 0 {
		return nil, errors.Join(failures...)
	}
	return nil, nil
}

func (p *OAuthProvider) saveClientsLocked() error {
	if p.clientsFile == "" {
		return nil
	}
	state := oauthClientsState{
		Version: oauthClientsStateVersion,
		Issuer:  p.publicBase,
		Clients: p.clients,
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode OAuth clients: %w", err)
	}
	data = append(data, '\n')
	return writeOAuthClientsAtomic(p.clientsFile, data)
}

func writeOAuthClientsAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create OAuth client directory: %w", err)
	}
	_ = os.Chmod(dir, 0o700)

	tmp, err := os.CreateTemp(dir, ".oauth-clients-*")
	if err != nil {
		return fmt.Errorf("create OAuth client temp file: %w", err)
	}
	tmpPath := tmp.Name()
	keepTmp := true
	defer func() {
		_ = tmp.Close()
		if keepTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod OAuth client temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write OAuth client temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync OAuth client temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close OAuth client temp file: %w", err)
	}

	backup := path + ".bak"
	hadPrimary := false
	if _, err := os.Stat(path); err == nil {
		hadPrimary = true
		_ = os.Remove(backup)
		if err := os.Rename(path, backup); err != nil {
			return fmt.Errorf("backup OAuth client state: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat OAuth client state: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		if hadPrimary {
			_ = os.Rename(backup, path)
		}
		return fmt.Errorf("install OAuth client state: %w", err)
	}
	keepTmp = false
	_ = os.Chmod(path, 0o600)
	_ = os.Remove(backup)
	return nil
}
