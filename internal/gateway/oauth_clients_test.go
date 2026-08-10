package gateway

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestOAuthClientRegistrationSurvivesProviderRestart(t *testing.T) {
	const base = "https://zshell.example.test"
	adminPIN := strings.Repeat("p", 24)
	jwtSecret := strings.Repeat("a", 64)
	stateFile := t.TempDir() + "/oauth-clients.json"

	p, err := NewPersistentOAuthProvider(base, adminPIN, jwtSecret, stateFile)
	if err != nil {
		t.Fatal(err)
	}

	body := `{"redirect_uris":["https://chatgpt.com/connector/oauth/test"],"client_name":"ChatGPT"}`
	req := httptest.NewRequest(http.MethodPost, base+"/oauth/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.HandleRegister(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register status=%d body=%s", rec.Code, rec.Body.String())
	}

	var registration struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &registration); err != nil {
		t.Fatal(err)
	}
	if registration.ClientID == "" {
		t.Fatal("registration returned an empty client_id")
	}

	// Simulate a complete Gateway restart: no shared in-memory registry.
	restarted, err := NewPersistentOAuthProvider(base, adminPIN, jwtSecret, stateFile)
	if err != nil {
		t.Fatal(err)
	}

	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	params := authorizeParams{
		ResponseType:        "code",
		ClientID:            registration.ClientID,
		RedirectURI:         "https://chatgpt.com/connector/oauth/test",
		CodeChallenge:       challenge,
		CodeChallengeMethod: "S256",
		Scope:               executeScope,
		Resource:            restarted.resourceURI(),
	}
	if err := restarted.validateAuthorize(params); err != nil {
		t.Fatalf("persisted OAuth client was not accepted after restart: %v", err)
	}

	stored, err := readOAuthClientsState(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.Clients[registration.ClientID].ID != registration.ClientID {
		t.Fatal("registered OAuth client was not persisted")
	}
}

func TestOAuthClientStateDoesNotContainOAuthSecrets(t *testing.T) {
	const base = "https://zshell.example.test"
	adminPIN := strings.Repeat("p", 24)
	jwtSecret := strings.Repeat("a", 64)
	stateFile := t.TempDir() + "/oauth-clients.json"

	p, err := NewPersistentOAuthProvider(base, adminPIN, jwtSecret, stateFile)
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.clients["zshell-test"] = oauthClient{
		ID:           "zshell-test",
		Name:         "ChatGPT",
		RedirectURIs: []string{"https://chatgpt.com/connector/oauth/test"},
	}
	if err := p.saveClientsLocked(); err != nil {
		p.mu.Unlock()
		t.Fatal(err)
	}
	p.mu.Unlock()

	data, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, adminPIN) || strings.Contains(text, jwtSecret) {
		t.Fatal("OAuth client state unexpectedly contains an OAuth secret")
	}
}
