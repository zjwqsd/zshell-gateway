package gateway

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	maxOAuthBodyBytes = 64 << 10
	maxClients        = 1024
	maxCodes          = 2048
	maxRedirectURIs   = 10
	maxURILength      = 2048
	maxClientName     = 200
	codeTTL           = 5 * time.Minute
)

type oauthClient struct {
	ID           string
	Name         string
	RedirectURIs []string
}

type authorizationCode struct {
	Code          string
	ClientID      string
	RedirectURI   string
	CodeChallenge string
	Scope         string
	Resource      string
	CreatedAt     time.Time
}

type OAuthProvider struct {
	publicBase string
	adminPIN   string
	jwtSecret  string

	mu      sync.Mutex
	clients map[string]oauthClient
	codes   map[string]authorizationCode
}

type authorizeParams struct {
	ResponseType        string
	ClientID            string
	RedirectURI         string
	CodeChallenge       string
	CodeChallengeMethod string
	Scope               string
	Resource            string
	State               string
}

func NewOAuthProvider(publicBase, adminPIN, jwtSecret string) *OAuthProvider {
	return &OAuthProvider{
		publicBase: publicBase,
		adminPIN:   adminPIN,
		jwtSecret:  jwtSecret,
		clients:    make(map[string]oauthClient),
		codes:      make(map[string]authorizationCode),
	}
}

func (p *OAuthProvider) resourceURI() string {
	return p.publicBase + "/mcp"
}

func (p *OAuthProvider) HandleProtectedResource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 p.resourceURI(),
		"resource_name":            "zshell MCP server",
		"authorization_servers":    []string{p.publicBase},
		"scopes_supported":         []string{executeScope},
		"bearer_methods_supported": []string{"header"},
	})
}

func (p *OAuthProvider) HandleAuthorizationServer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         p.publicBase,
		"authorization_endpoint":                         p.publicBase + "/oauth/authorize",
		"token_endpoint":                                 p.publicBase + "/oauth/token",
		"registration_endpoint":                          p.publicBase + "/oauth/register",
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"scopes_supported":                               []string{executeScope},
		"authorization_response_iss_parameter_supported": true,
		"resource_parameter_supported":                   true,
	})
}

func (p *OAuthProvider) HandleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !isMediaType(r.Header.Get("Content-Type"), "application/json") {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "Request body must be JSON")
		return
	}

	var input struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxOAuthBodyBytes))
	if err := decoder.Decode(&input); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "Request body must contain valid client metadata")
		return
	}
	if len(input.RedirectURIs) == 0 || len(input.RedirectURIs) > maxRedirectURIs {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "Provide between 1 and 10 redirect_uris")
		return
	}
	for _, redirectURI := range input.RedirectURIs {
		if !validRedirectURI(redirectURI) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uri must be an absolute URI without a fragment")
			return
		}
	}
	if len(input.ClientName) > maxClientName {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "client_name is too long")
		return
	}

	clientID, err := randomID("zshell-", 24)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	client := oauthClient{
		ID:           clientID,
		Name:         input.ClientName,
		RedirectURIs: append([]string(nil), input.RedirectURIs...),
	}

	p.mu.Lock()
	if len(p.clients) >= maxClients {
		p.mu.Unlock()
		writeOAuthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "OAuth client registry is full")
		return
	}
	p.clients[clientID] = client
	p.mu.Unlock()

	clientName := input.ClientName
	if clientName == "" {
		clientName = "ChatGPT"
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  clientID,
		"client_name":                clientName,
		"redirect_uris":              input.RedirectURIs,
		"grant_types":                []string{"authorization_code"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

func (p *OAuthProvider) HandleAuthorize(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		params, err := parseAuthorizeValues(r.URL.Query())
		if err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		p.renderAuthorizePage(w, params, p.validateAuthorize(params))

	case http.MethodPost:
		if !isMediaType(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			http.Error(w, "content-type must be application/x-www-form-urlencoded", http.StatusUnsupportedMediaType)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxOAuthBodyBytes)
		if err := r.ParseForm(); err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", "Authorization form is invalid")
			return
		}
		params, err := parseAuthorizeValues(r.PostForm)
		if err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if err := p.validateAuthorize(params); err != nil {
			p.renderAuthorizePage(w, params, err)
			return
		}
		pin, err := singleton(r.PostForm, "pin", true)
		if err != nil {
			p.renderAuthorizePage(w, params, fmt.Errorf("Missing admin PIN"))
			return
		}
		if !constantTimePINEqual(p.adminPIN, pin) {
			p.renderAuthorizePage(w, params, fmt.Errorf("Invalid admin PIN"))
			return
		}

		code, err := p.issueAuthorizationCode(params)
		if err != nil {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		location, err := url.Parse(params.RedirectURI)
		if err != nil {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		query := location.Query()
		query.Set("code", code)
		if params.State != "" {
			query.Set("state", params.State)
		}
		query.Set("iss", p.publicBase)
		location.RawQuery = query.Encode()
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, location.String(), http.StatusFound)

	default:
		methodNotAllowed(w)
	}
}

func (p *OAuthProvider) HandleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !isMediaType(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "Token request must be form encoded")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxOAuthBodyBytes)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "Token request is invalid")
		return
	}

	grantType, err := singleton(r.PostForm, "grant_type", true)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "Missing required OAuth token parameter")
		return
	}
	if grantType != "authorization_code" {
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "Only authorization_code is supported")
		return
	}
	code, e1 := singleton(r.PostForm, "code", true)
	clientID, e2 := singleton(r.PostForm, "client_id", true)
	redirectURI, e3 := singleton(r.PostForm, "redirect_uri", true)
	verifier, e4 := singleton(r.PostForm, "code_verifier", true)
	resource, e5 := singleton(r.PostForm, "resource", false)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "Missing required OAuth token parameter")
		return
	}
	if !validPKCEVerifier(verifier) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "Authorization code or PKCE verifier is invalid")
		return
	}
	if resource != "" && resource != p.resourceURI() {
		writeOAuthError(
			w,
			http.StatusBadRequest,
			"invalid_target",
			"OAuth resource does not match this zshell server",
		)
		return
	}

	token, err := p.exchangeAuthorizationCode(code, clientID, redirectURI, verifier)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "Authorization code or PKCE verifier is invalid")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": token,
		"token_type":   "Bearer",
		"scope":        executeScope,
	})
}

func (p *OAuthProvider) validateAuthorize(params authorizeParams) error {
	if params.ResponseType != "code" {
		return fmt.Errorf("Unsupported OAuth response_type")
	}
	if !validPKCEChallenge(params.CodeChallenge) || params.CodeChallengeMethod != "S256" {
		return fmt.Errorf("PKCE S256 is required")
	}
	if params.Scope != "" && params.Scope != executeScope {
		return fmt.Errorf("Unsupported OAuth scope")
	}
	if params.Resource != "" && params.Resource != p.resourceURI() {
		return fmt.Errorf("OAuth resource does not match this zshell server")
	}

	p.mu.Lock()
	client, ok := p.clients[params.ClientID]
	p.mu.Unlock()
	if !ok {
		return fmt.Errorf("Unknown OAuth client")
	}
	for _, redirectURI := range client.RedirectURIs {
		if redirectURI == params.RedirectURI {
			return nil
		}
	}
	return fmt.Errorf("OAuth redirect_uri does not match registered client")
}

func (p *OAuthProvider) issueAuthorizationCode(params authorizeParams) (string, error) {
	code, err := randomID("", 32)
	if err != nil {
		return "", err
	}
	entry := authorizationCode{
		Code:          code,
		ClientID:      params.ClientID,
		RedirectURI:   params.RedirectURI,
		CodeChallenge: params.CodeChallenge,
		Scope:         executeScope,
		Resource:      p.resourceURI(),
		CreatedAt:     time.Now(),
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.pruneCodesLocked()
	if len(p.codes) >= maxCodes {
		return "", fmt.Errorf("authorization code registry is full")
	}
	p.codes[code] = entry
	return code, nil
}

func (p *OAuthProvider) exchangeAuthorizationCode(code, clientID, redirectURI, verifier string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pruneCodesLocked()

	entry, ok := p.codes[code]
	if !ok || entry.ClientID != clientID || entry.RedirectURI != redirectURI || !verifyPKCE(entry.CodeChallenge, verifier) {
		return "", fmt.Errorf("invalid grant")
	}

	token, err := signToken(accessClaims{
		Issuer:   p.publicBase,
		Subject:  "zshell-user",
		Audience: entry.Resource,
		ClientID: entry.ClientID,
		Scope:    entry.Scope,
	}, p.jwtSecret)
	if err != nil {
		return "", err
	}
	delete(p.codes, code)
	return token, nil
}

func (p *OAuthProvider) pruneCodesLocked() {
	cutoff := time.Now().Add(-codeTTL)
	for code, entry := range p.codes {
		if entry.CreatedAt.Before(cutoff) {
			delete(p.codes, code)
		}
	}
}

var authorizePage = template.Must(template.New("authorize").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>Authorize zshell</title></head>
<body><main><h1>Authorize zshell</h1>
{{if .Error}}<p role="alert">{{.Error}}</p>{{end}}
<p>Approve access to the local zshell execution service.</p>
<form method="post" action="/oauth/authorize">
<input type="hidden" name="response_type" value="{{.Params.ResponseType}}">
<input type="hidden" name="client_id" value="{{.Params.ClientID}}">
<input type="hidden" name="redirect_uri" value="{{.Params.RedirectURI}}">
<input type="hidden" name="code_challenge" value="{{.Params.CodeChallenge}}">
<input type="hidden" name="code_challenge_method" value="{{.Params.CodeChallengeMethod}}">
<input type="hidden" name="scope" value="{{.Params.Scope}}">
<input type="hidden" name="resource" value="{{.Params.Resource}}">
<input type="hidden" name="state" value="{{.Params.State}}">
<label>Admin PIN <input type="password" name="pin" autocomplete="off" required></label>
<button type="submit">Authorize</button>
</form></main></body></html>`))

func (p *OAuthProvider) renderAuthorizePage(w http.ResponseWriter, params authorizeParams, validationError error) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	message := ""
	if validationError != nil {
		message = validationError.Error()
	}
	_ = authorizePage.Execute(w, struct {
		Params authorizeParams
		Error  string
	}{Params: params, Error: message})
}

func parseAuthorizeValues(values url.Values) (authorizeParams, error) {
	responseType, err := singleton(values, "response_type", true)
	if err != nil {
		return authorizeParams{}, fmt.Errorf("Missing required OAuth authorization parameter")
	}
	clientID, err := singleton(values, "client_id", true)
	if err != nil {
		return authorizeParams{}, fmt.Errorf("Missing required OAuth authorization parameter")
	}
	redirectURI, err := singleton(values, "redirect_uri", true)
	if err != nil {
		return authorizeParams{}, fmt.Errorf("Missing required OAuth authorization parameter")
	}
	challenge, err := singleton(values, "code_challenge", true)
	if err != nil {
		return authorizeParams{}, fmt.Errorf("Missing required OAuth authorization parameter")
	}
	challengeMethod, err := singleton(values, "code_challenge_method", true)
	if err != nil {
		return authorizeParams{}, fmt.Errorf("Missing required OAuth authorization parameter")
	}
	scope, err := singleton(values, "scope", false)
	if err != nil {
		return authorizeParams{}, fmt.Errorf("Authorization request contains duplicate parameters")
	}
	resource, err := singleton(values, "resource", false)
	if err != nil {
		return authorizeParams{}, fmt.Errorf("Authorization request contains duplicate parameters")
	}
	state, err := singleton(values, "state", false)
	if err != nil {
		return authorizeParams{}, fmt.Errorf("Authorization request contains duplicate parameters")
	}
	return authorizeParams{
		ResponseType:        responseType,
		ClientID:            clientID,
		RedirectURI:         redirectURI,
		CodeChallenge:       challenge,
		CodeChallengeMethod: challengeMethod,
		Scope:               scope,
		Resource:            resource,
		State:               state,
	}, nil
}

func singleton(values url.Values, key string, required bool) (string, error) {
	items, ok := values[key]
	if !ok || len(items) == 0 || (len(items) == 1 && items[0] == "") {
		if required {
			return "", fmt.Errorf("missing %s", key)
		}
		return "", nil
	}
	if len(items) != 1 {
		return "", fmt.Errorf("duplicate %s", key)
	}
	return items[0], nil
}

func validRedirectURI(raw string) bool {
	if raw == "" || len(raw) > maxURILength || strings.Contains(raw, "#") {
		return false
	}
	for _, r := range raw {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Scheme == "" || parsed.Fragment != "" {
		return false
	}
	if strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https") {
		return parsed.Host != "" && parsed.User == nil
	}
	return true
}

func validPKCEChallenge(value string) bool {
	if len(value) != 43 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func validPKCEVerifier(value string) bool {
	if len(value) < 43 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && !strings.ContainsRune("-._~", r) {
			return false
		}
	}
	return true
}

func verifyPKCE(expected, verifier string) bool {
	digest := sha256.Sum256([]byte(verifier))
	actual := base64.RawURLEncoding.EncodeToString(digest[:])
	return subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) == 1
}

func constantTimePINEqual(expected, presented string) bool {
	var left, right [512]byte
	copy(left[:], expected)
	copy(right[:], presented)
	sameBytes := subtle.ConstantTimeCompare(left[:], right[:])
	sameLength := subtle.ConstantTimeEq(int32(len(expected)), int32(len(presented)))
	return sameBytes&sameLength == 1
}

func randomID(prefix string, byteCount int) (string, error) {
	buffer := make([]byte, byteCount)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buffer), nil
}

func isMediaType(header, expected string) bool {
	mediaType, _, err := mime.ParseMediaType(header)
	return err == nil && strings.EqualFold(mediaType, expected)
}

func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, map[string]any{
		"error":             code,
		"error_description": description,
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func methodNotAllowed(w http.ResponseWriter) {
	writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
}
