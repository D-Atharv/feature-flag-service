// Command api runs the feature flag service's HTTP server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/D-Atharv/feature-flag-service/internal/config"
	"github.com/D-Atharv/feature-flag-service/internal/evaluation"
	"github.com/D-Atharv/feature-flag-service/internal/httpapi/handlers"
	"github.com/D-Atharv/feature-flag-service/internal/httpapi/middleware"
	"github.com/D-Atharv/feature-flag-service/internal/platform"
	store "github.com/D-Atharv/feature-flag-service/internal/store/postgres"
)

// Compile-time assertion: *store.FlagRepo must satisfy evaluation.FlagSource.
var _ evaluation.FlagSource = (*store.FlagRepo)(nil)

const (
	readHeaderTimeout  = 5 * time.Second
	shutdownTimeout    = 10 * time.Second
	healthcheckTimeout = 2 * time.Second
)

var (
	version   = "dev"
	gitSHA    = "unknown"
	buildTime = "unknown"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe /healthz and exit 0/1")
	flag.Parse()

	if *healthcheck {
		os.Exit(runHealthcheck())
	}

	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func runHealthcheck() int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: config:", err)
		return 1
	}
	client := http.Client{Timeout: healthcheckTimeout}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", cfg.Port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: request failed:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: unexpected status", resp.StatusCode)
		return 1
	}
	return 0
}

func run() error {
	log.Printf("feature-flag-service %s (sha=%s built=%s) starting", version, gitSHA, buildTime)

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	if cfg.Env == "prod" {
		gin.SetMode(gin.ReleaseMode)
	}

	startCtx := context.Background()
	pool, err := platform.NewPostgresPool(startCtx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()

	flagRepo := store.NewFlagRepo(pool)
	keyRepo := store.NewAPIKeyRepo(pool)

	// Load all active API keys into an in-memory map once at startup.
	// The hot path never touches the DB for auth — O(1) map lookup instead.
	apiKeys, err := keyRepo.List(startCtx)
	if err != nil {
		return fmt.Errorf("load api keys: %w", err)
	}
	keyMap := middleware.NewKeyMap(apiKeys)
	log.Printf("loaded %d active API key(s)", len(apiKeys))

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           newRouter(flagRepo, keyMap),
		ReadHeaderTimeout: readHeaderTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("listening on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		return fmt.Errorf("server error: %w", err)
	case <-ctx.Done():
		log.Println("shutdown signal received, draining in-flight requests")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	log.Println("shutdown complete")
	return nil
}

// newRouter assembles the Gin engine with the full middleware chain.
//
// Middleware order (each position is load-bearing — see BUILD-PLAN.md §9.2):
//
//	RequestID → Recovery → Logger → Metrics → BodyLimit → Timeout → Auth → handler
//
// /healthz and /metrics are registered before Use() so they bypass Auth.
func newRouter(flagRepo *store.FlagRepo, keyMap middleware.KeyMap) *gin.Engine {
	router := gin.New()

	// Ops endpoints bypass the auth middleware — registered before Use().
	router.GET("/healthz", healthz)
	router.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// Global chain applied to all routes registered after this point.
	router.Use(
		middleware.RequestIDWithSecurity(),
		middleware.Recovery(),
		middleware.Logger(),
		middleware.Metrics(),
		middleware.BodyLimit(),
		middleware.Timeout(),
		middleware.Auth(keyMap),
	)

	flagHandler := handlers.NewFlagHandler(flagRepo)
	evalHandler := handlers.NewEvalHandler(flagRepo)

	v1 := router.Group("/api/v1")

	// All flag routes require admin scope (non-admin keys are evaluate-only).
	flagV1 := v1.Group("/")
	flagV1.Use(middleware.RequireAdmin())
	flagV1.POST("/flags", flagHandler.Create)
	flagV1.GET("/flags", flagHandler.List)
	flagV1.GET("/flags/:key", flagHandler.Get)
	flagV1.PATCH("/flags/:key", flagHandler.Update)
	flagV1.DELETE("/flags/:key", flagHandler.Delete)

	// Evaluate routes — all authenticated keys (both admin and evaluate-scoped).
	root := router.Group("/")
	evalHandler.Register(root, v1)

	return router
}

func healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
