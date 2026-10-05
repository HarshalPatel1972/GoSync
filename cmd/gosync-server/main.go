// Command gosync-server runs the GoSync sync server.
//
// Configuration is read from environment variables:
//
//	GOSYNC_ADDR              listen address (default ":8080")
//	GOSYNC_DATABASE_URL      postgres://... URL or SQLite file path (default "gosync.db")
//	GOSYNC_JWT_SECRET        HS256 secret (>= 32 bytes), or
//	GOSYNC_JWKS_URL          JWKS URL of your identity provider
//	GOSYNC_JWT_ISSUER        required "iss" claim (optional)
//	GOSYNC_JWT_AUDIENCE      required "aud" claim (optional)
//	GOSYNC_NAMESPACE_CLAIM   claim that selects the data namespace (default "sub")
//	GOSYNC_INSECURE_DEV_AUTH "true" trusts the token as a user id. DEMO ONLY.
//	GOSYNC_ALLOWED_ORIGINS   comma-separated browser origins, or "*"
//	GOSYNC_STATIC_DIR        optional directory served at / (e.g. your web app)
//	GOSYNC_TLS_CERT, GOSYNC_TLS_KEY  serve HTTPS/WSS directly (otherwise terminate TLS at a proxy)
//	GOSYNC_LOG_FORMAT        "json" (default) or "text"
//	GOSYNC_LOG_LEVEL         debug, info (default), warn, error
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/HarshalPatel1972/GoSync/server"
	"github.com/HarshalPatel1972/GoSync/store/sqlstore"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func run() error {
	log := newLogger()
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn := env("GOSYNC_DATABASE_URL", "gosync.db")
	st, err := sqlstore.Open(ctx, dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer st.Close()

	auth, err := newAuth(ctx, log)
	if err != nil {
		return err
	}

	cfg := server.Config{Store: st, Auth: auth, Logger: log}
	if origins := env("GOSYNC_ALLOWED_ORIGINS", ""); origins != "" {
		for _, o := range strings.Split(origins, ",") {
			cfg.AllowedOrigins = append(cfg.AllowedOrigins, strings.TrimSpace(o))
		}
	}
	if sqlstore.IsPostgres(dsn) {
		broker := sqlstore.NewPostgresBroker(st, dsn, log)
		defer broker.Close()
		cfg.Broker = broker
		log.Info("using postgres LISTEN/NOTIFY for multi-instance fan-out")
	}
	srv, err := server.New(cfg)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("/sync", srv.Handler())
	mux.Handle("/healthz", srv.Handler())
	mux.Handle("/readyz", srv.Handler())
	if dir := env("GOSYNC_STATIC_DIR", ""); dir != "" {
		mux.Handle("/", staticHandler(dir))
		log.Info("serving static files", "dir", dir)
	}

	httpSrv := &http.Server{
		Addr:              env("GOSYNC_ADDR", ":8080"),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	cert, key := env("GOSYNC_TLS_CERT", ""), env("GOSYNC_TLS_KEY", "")
	errCh := make(chan error, 1)
	go func() {
		log.Info("gosync server listening", "addr", httpSrv.Addr, "tls", cert != "")
		if cert != "" {
			errCh <- httpSrv.ListenAndServeTLS(cert, key)
		} else {
			errCh <- httpSrv.ListenAndServe()
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("sync sessions did not close in time", "err", err)
	}
	if err := httpSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func newLogger() *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(env("GOSYNC_LOG_LEVEL", "info"))); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if env("GOSYNC_LOG_FORMAT", "json") == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}

func newAuth(ctx context.Context, log *slog.Logger) (server.Authenticator, error) {
	secret, jwks := env("GOSYNC_JWT_SECRET", ""), env("GOSYNC_JWKS_URL", "")
	dev := env("GOSYNC_INSECURE_DEV_AUTH", "") == "true"
	switch {
	case dev && (secret != "" || jwks != ""):
		return nil, errors.New("GOSYNC_INSECURE_DEV_AUTH cannot be combined with JWT settings")
	case dev:
		log.Warn("INSECURE DEV AUTH ENABLED: any client can access any user's data. Never use this in production.")
		return server.InsecureDevAuthenticator(), nil
	case secret == "" && jwks == "":
		return nil, errors.New("configure authentication: set GOSYNC_JWT_SECRET or GOSYNC_JWKS_URL (or GOSYNC_INSECURE_DEV_AUTH=true for local demos)")
	}
	return server.NewJWTAuthenticator(ctx, server.JWTConfig{
		Secret:         []byte(secret),
		JWKSURL:        jwks,
		Issuer:         env("GOSYNC_JWT_ISSUER", ""),
		Audience:       env("GOSYNC_JWT_AUDIENCE", ""),
		NamespaceClaim: env("GOSYNC_NAMESPACE_CLAIM", "sub"),
	})
}

// staticHandler serves dir with the right MIME type for .wasm and without
// caching, so redeploys are picked up.
func staticHandler(dir string) http.Handler {
	fs := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".wasm") {
			w.Header().Set("Content-Type", "application/wasm")
		}
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		fs.ServeHTTP(w, r)
	})
}
