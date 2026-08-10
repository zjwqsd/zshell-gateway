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
	oauthProvider := NewOAuthProvider(cfg.PublicBase, cfg.AdminPIN, cfg.JWTSecret)

	mcpServer := mcp.NewServer(&mcp.Implementation{
		Name:    "zshell",
		Version: "0.2.0",
	}, &mcp.ServerOptions{
		Instructions: "Provides terminal access through connected ShellCore devices over WebSocket. Device names are declared by each ShellCore and update dynamically as devices connect or disconnect. Call device_list to inspect current devices and workspaces. When multiple devices are online, select one explicitly with the device argument. Local human control on each ShellCore may block new mutating actions or terminate active work.",
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
	slog.Info("zshell ShellCore WebSocket endpoint", "path", "/device/ws")
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
