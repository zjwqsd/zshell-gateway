package gateway

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

func TestPKCES256(t *testing.T) {
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	if !verifyPKCE(challenge, verifier) {
		t.Fatal("valid PKCE verifier was rejected")
	}
	if verifyPKCE(challenge, strings.Repeat("A", 64)) {
		t.Fatal("invalid PKCE verifier was accepted")
	}
}

func TestRedirectURIs(t *testing.T) {
	valid := []string{
		"https://chatgpt.com/oauth/callback",
		"http://127.0.0.1:9876/callback",
		"com.example.app:/oauth/callback",
	}
	for _, uri := range valid {
		if !validRedirectURI(uri) {
			t.Fatalf("valid redirect URI rejected: %s", uri)
		}
	}
	invalid := []string{
		"/relative/callback",
		"https:///missing-host",
		"https://example.com/callback#fragment",
	}
	for _, uri := range invalid {
		if validRedirectURI(uri) {
			t.Fatalf("invalid redirect URI accepted: %s", uri)
		}
	}
}
