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
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	goredis "github.com/redis/go-redis/v9"

	"github.com/D-Atharv/feature-flag-service/internal/config"
	"github.com/D-Atharv/feature-flag-service/internal/evaluation"
	apidocs "github.com/D-Atharv/feature-flag-service/internal/httpapi/docs"
	"github.com/D-Atharv/feature-flag-service/internal/httpapi/handlers"
	"github.com/D-Atharv/feature-flag-service/internal/httpapi/middleware"
	"github.com/D-Atharv/feature-flag-service/internal/platform"
	"github.com/D-Atharv/feature-flag-service/internal/ratelimit"
	rlmemory "github.com/D-Atharv/feature-flag-service/internal/ratelimit/memory"
	rlredis "github.com/D-Atharv/feature-flag-service/internal/ratelimit/redis"
	"github.com/D-Atharv/feature-flag-service/internal/snapshot"
	store "github.com/D-Atharv/feature-flag-service/internal/store/postgres"
)

// Both satisfy evaluation.FlagSource; /evaluate reads the snapshot.
var (
	_ evaluation.FlagSource = (*store.FlagRepo)(nil)
	_ evaluation.FlagSource = (*snapshot.Store)(nil)
	_ snapshot.Loader       = (*store.FlagRepo)(nil)
)

const (
	readHeaderTimeout      = 5 * time.Second
	shutdownTimeout        = 10 * time.Second
	healthcheckTimeout     = 2 * time.Second
	dependencyProbeTimeout = 2 * time.Second
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

	// Rate limiter. Redis holds the authoritative buckets so quota outlives
	// this process; the in-memory limiter is the fallback the circuit breaker
	// routes to when Redis is unreachable. Constructing it never fails —
	// a limiter that can refuse to start is not one that fails open.
	redisClient := platform.NewRedisClient(cfg.RedisAddr)
	defer func() { _ = redisClient.Close() }()
	limiter := ratelimit.NewBreaker(rlredis.New(redisClient), rlmemory.New())

	// In-memory flag snapshot: /evaluate reads this, never the database, so
	// evaluations keep serving with Postgres down.
	flags := snapshot.New()
	refresher := snapshot.NewRefresher(flags, flagRepo).
		WithListener(platform.NewListenerConnector(cfg.DatabaseURL))

	// Load before serving, so no request can arrive ahead of the snapshot.
	// Not fatal on failure: the service starts, /readyz reports not ready, and
	// the poller keeps trying — a database outage must not block a boot.
	if err := refresher.Prime(startCtx); err != nil {
		log.Printf("warning: initial flag snapshot load failed: %v", err)
	}

	bgCtx, stopBackground := context.WithCancel(context.Background())
	defer stopBackground()

	var bg sync.WaitGroup
	bg.Add(1)
	go func() {
		defer bg.Done()
		refresher.Run(bgCtx)
	}()

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           newRouter(flagRepo, flags, keyMap, limiter, pool, redisClient),
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

	// Background work stops only after in-flight requests have drained.
	stopBackground()
	bg.Wait()

	log.Println("shutdown complete")
	return nil
}

// newRouter assembles the Gin engine with the full middleware chain.
//
// Middleware order (each position is load-bearing — see BUILD-PLAN.md §9.2):
//
//	RequestID → Recovery → Logger → Metrics → BodyLimit → Timeout → Auth → RateLimit → handler
//
// RateLimit sits after Auth because a bucket is keyed on the resolved API key
// ID, and last overall because everything before it is bounded and cheap while
// the handler is the expensive thing being protected.
//
// /healthz, /readyz and /metrics are registered before Use() so they bypass
// both Auth and the limiter — a probe that can be rate limited eventually
// restarts a healthy instance.
func newRouter(
	flagRepo *store.FlagRepo,
	flags *snapshot.Store,
	keyMap middleware.KeyMap,
	limiter ratelimit.Limiter,
	pool *pgxpool.Pool,
	redisClient *goredis.Client,
) *gin.Engine {
	router := gin.New()

	// Ops and documentation bypass the auth middleware — registered before
	// Use(). An interviewer opening the bare URL must reach something useful,
	// and a 401 is as unhelpful as a 404.
	router.GET("/", index)
	router.GET("/healthz", healthz)
	router.GET("/readyz", readyz(flags))
	router.GET("/metrics", gin.WrapH(promhttp.Handler()))
	apidocs.Register(router)

	// Global chain applied to all routes registered after this point.
	router.Use(
		middleware.RequestIDWithSecurity(),
		middleware.Recovery(),
		middleware.Logger(),
		middleware.Metrics(),
		middleware.BodyLimit(),
		middleware.Timeout(),
		middleware.Auth(keyMap),
		middleware.RateLimit(limiter),
	)

	flagHandler := handlers.NewFlagHandler(flagRepo)
	// Writes go to Postgres; reads come from the snapshot.
	evalHandler := handlers.NewEvalHandler(flags)

	v1 := router.Group("/api/v1")

	// All flag routes require admin scope (non-admin keys are evaluate-only).
	flagV1 := v1.Group("/")
	flagV1.Use(middleware.RequireAdmin())
	flagV1.POST("/flags", flagHandler.Create)
	flagV1.GET("/flags", flagHandler.List)
	flagV1.GET("/flags/:key", flagHandler.Get)
	flagV1.PATCH("/flags/:key", flagHandler.Update)
	flagV1.DELETE("/flags/:key", flagHandler.Delete)

	// Diagnostic detail, admin only. Kept off /healthz and /readyz so a
	// dependency blip can never fail a probe.
	debugGroup := router.Group("/debug")
	debugGroup.Use(middleware.RequireAdmin())
	debugGroup.GET("/status", debugStatus(flags, pool, redisClient))

	// Evaluate routes — all authenticated keys (both admin and evaluate-scoped).
	root := router.Group("/")
	evalHandler.Register(root, v1)

	return router
}

// index answers the bare URL with what is running and where to go next.
func index(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"service":    "feature-flag-service",
		"version":    version,
		"git_sha":    gitSHA,
		"build_time": buildTime,
		"docs":       "/docs",
		"health":     "/healthz",
		"ready":      "/readyz",
		"metrics":    "/metrics",
		"evaluate":   "/evaluate/{key}?env={env}&subject={subject}",
	})
}

// healthz is liveness and checks nothing external. If it probed Postgres, a
// brief database blip would restart every healthy instance.
func healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// readyz gates on the flag snapshot, not on Postgres: with the snapshot loaded
// this instance serves evaluations whether or not the database is up.
func readyz(flags *snapshot.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !flags.Loaded() {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"status": "not ready",
				"reason": "flag snapshot not loaded",
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"status":       "ready",
			"flags":        flags.Len(),
			"last_refresh": flags.LastRefresh().UTC(),
		})
	}
}

// debugStatus reports dependency health for operators.
func debugStatus(flags *snapshot.Store, pool *pgxpool.Pool, redisClient *goredis.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), dependencyProbeTimeout)
		defer cancel()

		state := func(ok bool) string {
			if ok {
				return "ok"
			}
			return "degraded"
		}

		c.JSON(http.StatusOK, gin.H{
			"version":  version,
			"git_sha":  gitSHA,
			"postgres": state(pool != nil && pool.Ping(ctx) == nil),
			"redis":    state(redisClient != nil && redisClient.Ping(ctx).Err() == nil),
			"snapshot": gin.H{
				"loaded":       flags.Loaded(),
				"flags":        flags.Len(),
				"last_refresh": flags.LastRefresh().UTC(),
			},
		})
	}
}
