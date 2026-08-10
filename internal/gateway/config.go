package gateway

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const defaultMCPListen = "127.0.0.1:8765"

type Config struct {
	ListenAddr       string
	DeviceToken      string
	PublicBase       string
	AdminPIN         string
	JWTSecret        string
	OAuthClientsFile string
}

func LoadConfig() (Config, error) {
	listenAddr := strings.TrimSpace(os.Getenv("ZSHELL_MCP_LISTEN"))
	if listenAddr == "" {
		listenAddr = defaultMCPListen
	}
	if _, _, err := net.SplitHostPort(listenAddr); err != nil {
		return Config{}, fmt.Errorf("ZSHELL_MCP_LISTEN must be host:port: %w", err)
	}

	deviceToken := strings.TrimSpace(os.Getenv("ZSHELL_DEVICE_TOKEN"))
	if len(deviceToken) < 24 || len(deviceToken) > 512 {
		return Config{}, fmt.Errorf("ZSHELL_DEVICE_TOKEN must contain between 24 and 512 characters")
	}

	publicBase := strings.TrimRight(strings.TrimSpace(os.Getenv("ZSHELL_PUBLIC_BASE_URL")), "/")
	if publicBase == "" {
		return Config{}, fmt.Errorf("ZSHELL_PUBLIC_BASE_URL is required")
	}
	publicURL, err := url.Parse(publicBase)
	if err != nil {
		return Config{}, fmt.Errorf("parse ZSHELL_PUBLIC_BASE_URL: %w", err)
	}
	if publicURL.Scheme != "https" && !isLoopbackHTTP(publicURL) {
		return Config{}, fmt.Errorf("ZSHELL_PUBLIC_BASE_URL must use https, except loopback http for local testing")
	}
	if publicURL.Host == "" || publicURL.User != nil || publicURL.RawQuery != "" || publicURL.Fragment != "" || (publicURL.Path != "" && publicURL.Path != "/") {
		return Config{}, fmt.Errorf("ZSHELL_PUBLIC_BASE_URL must be an origin without path, credentials, query, or fragment")
	}

	adminPIN := os.Getenv("ZSHELL_OAUTH_ADMIN_PIN")
	if len(adminPIN) < 24 || len(adminPIN) > 512 {
		return Config{}, fmt.Errorf("ZSHELL_OAUTH_ADMIN_PIN must contain between 24 and 512 characters")
	}

	jwtSecret := strings.TrimSpace(os.Getenv("ZSHELL_OAUTH_JWT_SECRET"))
	if len(jwtSecret) != 64 || !isHex(jwtSecret) {
		return Config{}, fmt.Errorf("ZSHELL_OAUTH_JWT_SECRET must contain exactly 64 hexadecimal characters")
	}

	oauthClientsFile := strings.TrimSpace(os.Getenv("ZSHELL_OAUTH_CLIENTS_FILE"))
	if oauthClientsFile == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return Config{}, fmt.Errorf("resolve user config directory for OAuth clients: %w", err)
		}
		oauthClientsFile = filepath.Join(configDir, "zshell-gateway", "oauth-clients.json")
	}
	oauthClientsFile, err = filepath.Abs(oauthClientsFile)
	if err != nil {
		return Config{}, fmt.Errorf("resolve ZSHELL_OAUTH_CLIENTS_FILE: %w", err)
	}

	return Config{
		ListenAddr:       listenAddr,
		DeviceToken:      deviceToken,
		PublicBase:       publicBase,
		AdminPIN:         adminPIN,
		JWTSecret:        jwtSecret,
		OAuthClientsFile: oauthClientsFile,
	}, nil
}

func isLoopbackHTTP(u *url.URL) bool {
	if u.Scheme != "http" || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

func isHex(value string) bool {
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}
