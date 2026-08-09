package gateway

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

const (
	defaultMCPListen    = "127.0.0.1:8765"
	defaultDeviceListen = "0.0.0.0:8767"
)

type Config struct {
	ListenAddr   string
	DeviceListen string
	DeviceToken  string
	PublicBase   string
	AdminPIN     string
	JWTSecret    string
}

func LoadConfig() (Config, error) {
	listenAddr := strings.TrimSpace(os.Getenv("ZSHELL_MCP_LISTEN"))
	if listenAddr == "" {
		listenAddr = defaultMCPListen
	}
	if _, _, err := net.SplitHostPort(listenAddr); err != nil {
		return Config{}, fmt.Errorf("ZSHELL_MCP_LISTEN must be host:port: %w", err)
	}

	deviceListen := strings.TrimSpace(os.Getenv("ZSHELL_DEVICE_LISTEN"))
	if deviceListen == "" {
		deviceListen = defaultDeviceListen
	}
	if _, _, err := net.SplitHostPort(deviceListen); err != nil {
		return Config{}, fmt.Errorf("ZSHELL_DEVICE_LISTEN must be host:port: %w", err)
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

	return Config{
		ListenAddr:   listenAddr,
		DeviceListen: deviceListen,
		DeviceToken:  deviceToken,
		PublicBase:   publicBase,
		AdminPIN:     adminPIN,
		JWTSecret:    jwtSecret,
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
