package gateway

import (
	"context"
	"strings"
	"testing"
)

func TestAccessTokenRoundTrip(t *testing.T) {
	const base = "https://zshell.example.test"
	secret := strings.Repeat("a", 64)
	token, err := signToken(accessClaims{
		Issuer:   base,
		Subject:  "zshell-user",
		Audience: base + "/mcp",
		ClientID: "test-client",
		Scope:    executeScope,
	}, secret)
	if err != nil {
		t.Fatal(err)
	}
	verifier := NewTokenVerifier(base, secret)
	info, err := verifier(context.Background(), token, nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.UserID != "zshell-user" {
		t.Fatalf("unexpected subject %q", info.UserID)
	}
	if len(info.Scopes) != 1 || info.Scopes[0] != executeScope {
		t.Fatalf("unexpected scopes %#v", info.Scopes)
	}
}

func TestVerifierRejectsTampering(t *testing.T) {
	const base = "https://zshell.example.test"
	secret := strings.Repeat("b", 64)
	token, err := signToken(accessClaims{
		Issuer:   base,
		Subject:  "zshell-user",
		Audience: base + "/mcp",
		ClientID: "test-client",
		Scope:    executeScope,
	}, secret)
	if err != nil {
		t.Fatal(err)
	}
	verifier := NewTokenVerifier(base, secret)
	_, err = verifier(context.Background(), token+"x", nil)
	if err == nil {
		t.Fatal("tampered token was accepted")
	}
}
