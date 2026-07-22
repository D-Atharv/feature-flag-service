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

	"github.com/D-Atharv/feature-flag-service/internal/config"
	"github.com/D-Atharv/feature-flag-service/internal/httpapi/handlers"
	"github.com/D-Atharv/feature-flag-service/internal/platform"
	store "github.com/D-Atharv/feature-flag-service/internal/store/postgres"
)

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
	healthcheck := flag.Bool("healthcheck", false, "probe /healthz on the configured PORT and exit 0/1 (used as the Docker HEALTHCHECK; distroless has no shell/curl to do this itself)")
	flag.Parse()

	if *healthcheck {
		os.Exit(runHealthcheck())
	}

	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// runHealthcheck is a self-contained mode, not the running server: it makes
// one request to its own /healthz and reports success/failure via exit code.
// Distroless images have no shell, curl, or wget for a Docker HEALTHCHECK to
// invoke, so the binary has to be able to check itself.
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

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           newRouter(flagRepo),
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

// newRouter wires gin.New(), not gin.Default() — the default engine injects
// Gin's own logger and recovery middleware
func newRouter(flagRepo *store.FlagRepo) *gin.Engine {
	router := gin.New()
	router.GET("/healthz", healthz)

	v1 := router.Group("/api/v1")
	handlers.NewFlagHandler(flagRepo).Register(v1)

	return router
}

func healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
