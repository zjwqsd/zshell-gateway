package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"zshell-gateway/internal/device"
)

type Server struct {
	cfg        Config
	devices    *device.Manager
	oauth      *OAuthProvider
	httpServer *http.Server
}

func NewServer(cfg Config) (*Server, error) {
	devices := device.NewManager()
	oauthProvider, err := NewPersistentOAuthProvider(cfg.PublicBase, cfg.AdminPIN, cfg.JWTSecret, cfg.OAuthClientsFile)
	if err != nil {
		return nil, err
	}

	mcpServer := mcp.NewServer(&mcp.Implementation{
		Name:    "zshell",
		Version: "0.2.0",
	}, &mcp.ServerOptions{
		Instructions: "Provides terminal, filesystem, process, application, screen, native UI, browser, and direct cross-device file-transfer control through connected ShellCore devices over WebSocket or HTTP transport. Device names and capabilities are declared by each ShellCore and update dynamically as devices connect or disconnect. Call device_list to inspect current devices, workspaces, platforms, and capabilities. When multiple devices are online, select one explicitly with the device argument. For browser work, call browser_status first; enabled=false means that ShellCore was started without browser capability and browser tools other than status will return BrowserFeatureDisabled. When enabled, call browser_start when no session is active, and browser_snapshot to obtain current element refs before ref-based actions. Browser ownership may be transferred to the human only for a visible session; after ownership returns to the agent, take a fresh browser_snapshot before reusing refs. For cross-device files, use file_transfer with explicit sourceDevice/sourcePath and targetDevice/targetPath, then file_transfer_status or file_transfer_cancel. File bytes stream directly through Gateway transport endpoints rather than MCP content. Local human control on each ShellCore may block new mutating actions or terminate active work.",
	})
	RegisterTools(mcpServer, devices)

	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return mcpServer },
		&mcp.StreamableHTTPOptions{
			Stateless:                    true,
			JSONResponse:                 true,
			DisableLocalhostProtection:   true,
			MaxRequestBodyBytes:          1 << 20,
			PropagateRequestCancellation: true,
		},
	)

	authedMCP := mcpauth.RequireBearerToken(
		NewTokenVerifier(cfg.PublicBase, cfg.JWTSecret),
		&mcpauth.RequireBearerTokenOptions{
			ResourceMetadataURL:    cfg.PublicBase + "/.well-known/oauth-protected-resource/mcp",
			Scopes:                 []string{executeScope},
			AllowMissingExpiration: true,
		},
	)(mcpHandler)

	mux := http.NewServeMux()
	mux.Handle("/device/ws", device.NewWebSocketHandler(cfg.DeviceToken, devices))
	httpDeviceHandler := device.NewHTTPHandler(cfg.DeviceToken, devices)
	mux.Handle("/device/http", httpDeviceHandler)
	mux.Handle("/device/http/", httpDeviceHandler)
	mux.Handle("/mcp", logRequest("mcp", authedMCP))
	mux.HandleFunc("/.well-known/oauth-protected-resource", oauthProvider.HandleProtectedResource)
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", oauthProvider.HandleProtectedResource)
	mux.HandleFunc("/.well-known/oauth-authorization-server", oauthProvider.HandleAuthorizationServer)
	mux.HandleFunc("/.well-known/oauth-authorization-server/mcp", oauthProvider.HandleAuthorizationServer)
	mux.HandleFunc("/oauth/register", oauthProvider.HandleRegister)
	mux.HandleFunc("/oauth/authorize", oauthProvider.HandleAuthorize)
	mux.HandleFunc("/oauth/token", oauthProvider.HandleToken)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}

	return &Server{
		cfg:        cfg,
		devices:    devices,
		oauth:      oauthProvider,
		httpServer: httpServer,
	}, nil
}

func (s *Server) ListenAndServe() error {
	slog.Info("zshell gateway listening", "address", "http://"+s.cfg.ListenAddr)
	slog.Info("zshell public base", "url", s.cfg.PublicBase)
	slog.Info("zshell OAuth client registry", "file", s.cfg.OAuthClientsFile)
	slog.Info("zshell ShellCore WebSocket endpoint", "path", "/device/ws")
	slog.Info("zshell ShellCore HTTP endpoint", "path", "/device/http")
	return s.httpServer.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.devices.Close()
	return s.httpServer.Shutdown(ctx)
}

func logRequest(kind string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hasBearer := r.Header.Get("Authorization") != ""
		slog.Info("HTTP request", "kind", kind, "method", r.Method, "path", r.URL.Path, "authorization", hasBearer)
		next.ServeHTTP(w, r)
	})
}

func Run() error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	server, err := NewServer(cfg)
	if err != nil {
		return err
	}

	err = server.ListenAndServe()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func init() {
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(handler))
}
