package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/D-Atharv/feature-flag-service/internal/config"
)

func setValid(t *testing.T) {
	t.Helper()
	t.Setenv("ENV", "dev")
	t.Setenv("PORT", "8080")
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/db")
	t.Setenv("REDIS_ADDR", "localhost:6379")
	t.Setenv("LOG_LEVEL", "info")
}

func TestLoad_AllValid(t *testing.T) {
	setValid(t)

	cfg, err := config.Load()

	require.NoError(t, err)
	assert.Equal(t, config.Config{
		Env:         "dev",
		Port:        8080,
		DatabaseURL: "postgres://user:pass@localhost:5432/db",
		RedisAddr:   "localhost:6379",
		LogLevel:    "info",
	}, cfg)
}

func TestLoad_FailsFast(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(t *testing.T)
		wantErr string
	}{
		{"missing ENV", func(t *testing.T) { t.Setenv("ENV", "") }, "ENV must be"},
		{"invalid ENV", func(t *testing.T) { t.Setenv("ENV", "production") }, "ENV must be"},
		{"missing PORT", func(t *testing.T) { t.Setenv("PORT", "") }, "PORT must be"},
		{"non-numeric PORT", func(t *testing.T) { t.Setenv("PORT", "abc") }, "PORT must be"},
		{"out-of-range PORT", func(t *testing.T) { t.Setenv("PORT", "70000") }, "PORT must be"},
		{"missing DATABASE_URL", func(t *testing.T) { t.Setenv("DATABASE_URL", "") }, "DATABASE_URL must be set"},
		{"missing REDIS_ADDR", func(t *testing.T) { t.Setenv("REDIS_ADDR", "") }, "REDIS_ADDR must be set"},
		{"missing LOG_LEVEL", func(t *testing.T) { t.Setenv("LOG_LEVEL", "") }, "LOG_LEVEL must be"},
		{"invalid LOG_LEVEL", func(t *testing.T) { t.Setenv("LOG_LEVEL", "trace") }, "LOG_LEVEL must be"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setValid(t)
			tt.mutate(t)

			_, err := config.Load()

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestLoad_CollectsAllErrorsAtOnce(t *testing.T) {
	t.Setenv("ENV", "")
	t.Setenv("PORT", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("REDIS_ADDR", "")
	t.Setenv("LOG_LEVEL", "")

	_, err := config.Load()

	require.Error(t, err)
	for _, want := range []string{"ENV must be", "PORT must be", "DATABASE_URL must be set", "REDIS_ADDR must be set", "LOG_LEVEL must be"} {
		assert.Contains(t, err.Error(), want)
	}
}
