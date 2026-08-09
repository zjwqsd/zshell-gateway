package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
)

const jwtHeaderSegment = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"
const executeScope = "zshell:execute"

type accessClaims struct {
	Issuer   string `json:"iss"`
	Subject  string `json:"sub"`
	Audience string `json:"aud"`
	ClientID string `json:"client_id"`
	Scope    string `json:"scope"`
}

func NewTokenVerifier(publicBase, secret string) mcpauth.TokenVerifier {
	return func(_ context.Context, token string, _ *http.Request) (*mcpauth.TokenInfo, error) {
		claims, err := verifyToken(token, publicBase, secret)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", mcpauth.ErrInvalidToken, err)
		}
		return &mcpauth.TokenInfo{
			Scopes: strings.Fields(claims.Scope),
			UserID: claims.Subject,
			Extra: map[string]any{
				"client_id": claims.ClientID,
				"issuer":    claims.Issuer,
			},
		}, nil
	}
}

func signToken(claims accessClaims, secret string) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal JWT claims: %w", err)
	}
	payloadSegment := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := jwtHeaderSegment + "." + payloadSegment
	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := mac.Write([]byte(signingInput)); err != nil {
		return "", fmt.Errorf("sign JWT: %w", err)
	}
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signingInput + "." + signature, nil
}

func verifyToken(token, publicBase, secret string) (accessClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return accessClaims{}, errors.New("malformed JWT")
	}
	if parts[0] != jwtHeaderSegment {
		return accessClaims{}, errors.New("unexpected JWT header")
	}

	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := mac.Write([]byte(parts[0] + "." + parts[1])); err != nil {
		return accessClaims{}, fmt.Errorf("verify JWT signature: %w", err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return accessClaims{}, errors.New("malformed JWT signature")
	}
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return accessClaims{}, errors.New("invalid JWT signature")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return accessClaims{}, errors.New("malformed JWT payload")
	}
	var claims accessClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return accessClaims{}, errors.New("invalid JWT claims")
	}
	if claims.Issuer != publicBase {
		return accessClaims{}, errors.New("JWT issuer mismatch")
	}
	if claims.Audience != publicBase+"/mcp" {
		return accessClaims{}, errors.New("JWT audience mismatch")
	}
	if claims.Subject == "" || claims.ClientID == "" {
		return accessClaims{}, errors.New("JWT subject or client_id is empty")
	}
	if claims.Scope != executeScope {
		return accessClaims{}, errors.New("JWT scope mismatch")
	}
	return claims, nil
}
