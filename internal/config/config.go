package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds the service's runtime configuration, read once at startup.
type Config struct {
	Env         string
	Port        int
	DatabaseURL string
	RedisAddr   string
	LogLevel    string
}

var validEnvs = map[string]bool{"dev": true, "staging": true, "prod": true}

var validLogLevels = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}

func Load() (Config, error) {
	var errs []string

	env := os.Getenv("ENV")
	if !validEnvs[env] {
		errs = append(errs, fmt.Sprintf("ENV must be one of dev|staging|prod, got %q", env))
	}

	portRaw := os.Getenv("PORT")
	port, err := strconv.Atoi(portRaw)
	if err != nil || port < 1 || port > 65535 {
		errs = append(errs, fmt.Sprintf("PORT must be a valid port number 1-65535, got %q", portRaw))
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		errs = append(errs, "DATABASE_URL must be set")
	}

	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		errs = append(errs, "REDIS_ADDR must be set")
	}

	logLevel := os.Getenv("LOG_LEVEL")
	if !validLogLevels[logLevel] {
		errs = append(errs, fmt.Sprintf("LOG_LEVEL must be one of debug|info|warn|error, got %q", logLevel))
	}

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(errs, "\n  - "))
	}

	return Config{
		Env:         env,
		Port:        port,
		DatabaseURL: databaseURL,
		RedisAddr:   redisAddr,
		LogLevel:    logLevel,
	}, nil
}
