// Command gosync-server runs the GoSync sync server.
//
//	gosync-server [serve]                      run the server (default)
//	gosync-server migrate                      apply database migrations and exit
//	gosync-server compact [-older-than 720h]   purge old tombstones once and exit
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
//	GOSYNC_METRICS_ADDR      private ops listener: Prometheus /metrics and /debug/pprof, e.g. "127.0.0.1:9090" (off by default)
//	GOSYNC_TOMBSTONE_RETENTION  purge deleted documents after this long (default "720h"; "0" disables)
//	GOSYNC_LOG_FORMAT        "json" (default) or "text"
//	GOSYNC_LOG_LEVEL         debug, info (default), warn, error
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/HarshalPatel1972/GoSync/hlc"
	"github.com/HarshalPatel1972/GoSync/server"
	"github.com/HarshalPatel1972/GoSync/store/sqlstore"
)

func main() {
	var err error
	switch cmd := subcommand(); cmd {
	case "", "serve":
		err = run()
	case "migrate":
		err = migrateOnly()
	case "compact":
		err = compactOnce(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\nusage: gosync-server [serve | migrate | compact -older-than 720h]\n", cmd)
		os.Exit(2)
	}
	if err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func subcommand() string {
	if len(os.Args) > 1 {
		return os.Args[1]
	}
	return ""
}

// migrateOnly applies schema migrations and exits (for deploy pipelines).
func migrateOnly() error {
	slog.SetDefault(newLogger())
	st, err := sqlstore.Open(context.Background(), env("GOSYNC_DATABASE_URL", "gosync.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	v, err := st.SchemaVersion(context.Background())
	slog.Info("database schema is up to date", "version", v)
	return err
}

// compactOnce runs tombstone compaction to completion and exits. Connected
// clients learn about it on their next pull.
func compactOnce(args []string) error {
	fs := flag.NewFlagSet("compact", flag.ExitOnError)
	olderThan := fs.Duration("older-than", 720*time.Hour, "purge documents deleted longer ago than this")
	fs.Parse(args)
	log := newLogger()
	st, err := sqlstore.Open(context.Background(), env("GOSYNC_DATABASE_URL", "gosync.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	cutoff := hlc.Timestamp{Wall: time.Now().Add(-*olderThan).UnixMilli()}.String()
	total := 0
	for {
		n, _, err := st.Compact(context.Background(), cutoff, 500)
		total += n
		if err != nil {
			return err
		}
		if n < 500 {
			break
		}
	}
	log.Info("compaction finished", "purged_documents", total)
	return nil
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
	if addr := env("GOSYNC_METRICS_ADDR", ""); addr != "" {
		reg := prometheus.NewRegistry()
		reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
		cfg.Metrics = server.NewMetrics(reg)
		// The metrics listener is private (bind it to an internal interface),
		// so it also carries the Go profiler for diagnosing production issues.
		opsMux := http.NewServeMux()
		opsMux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
		opsMux.HandleFunc("/debug/pprof/", pprof.Index)
		opsMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		opsMux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		metricsSrv := &http.Server{Addr: addr, Handler: opsMux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			log.Info("serving metrics", "addr", addr)
			if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("metrics server failed", "err", err)
			}
		}()
		defer metricsSrv.Close()
	}
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

	retention, err := time.ParseDuration(env("GOSYNC_TOMBSTONE_RETENTION", "720h"))
	if err != nil {
		return fmt.Errorf("GOSYNC_TOMBSTONE_RETENTION: %w", err)
	}
	if retention > 0 {
		if retention < 24*time.Hour {
			return errors.New("GOSYNC_TOMBSTONE_RETENTION must be at least 24h: devices offline longer than this may hold deleted data")
		}
		go srv.RunCompaction(ctx, retention, time.Hour)
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

// staticHandler serves dir. For a file with a precompressed sibling
// (file.br or file.gz) it sends that when the client accepts it; the browser
// SDK ships gosync.wasm.br, cutting the download by about 80%.
func staticHandler(dir string) http.Handler {
	root := http.Dir(dir)
	fs := http.FileServer(root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-cache") // revalidate so redeploys are picked up
		if strings.HasSuffix(r.URL.Path, ".wasm") {
			h.Set("Content-Type", "application/wasm")
		}
		h.Add("Vary", "Accept-Encoding")
		accept := r.Header.Get("Accept-Encoding")
		for _, enc := range []struct{ token, ext string }{{"br", ".br"}, {"gzip", ".gz"}} {
			if !acceptsEncoding(accept, enc.token) {
				continue
			}
			f, err := root.Open(r.URL.Path + enc.ext)
			if err != nil {
				continue
			}
			info, err := f.Stat()
			if err != nil || info.IsDir() {
				f.Close()
				continue
			}
			if h.Get("Content-Type") == "" {
				if ct := mime.TypeByExtension(path.Ext(r.URL.Path)); ct != "" {
					h.Set("Content-Type", ct)
				}
			}
			h.Set("Content-Encoding", enc.token)
			http.ServeContent(w, r, r.URL.Path, info.ModTime(), f)
			f.Close()
			return
		}
		fs.ServeHTTP(w, r)
	})
}

func acceptsEncoding(header, token string) bool {
	for _, part := range strings.Split(header, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(name), token) {
			continue
		}
		if q, ok := strings.CutPrefix(strings.TrimSpace(params), "q="); ok {
			v, err := strconv.ParseFloat(q, 64)
			return err == nil && v > 0
		}
		return true
	}
	return false
}
