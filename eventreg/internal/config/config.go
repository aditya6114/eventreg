// Package config loads all runtime settings from environment variables, in
// ONE place, at startup.
//
// THE 12-FACTOR PRINCIPLE (checklist interview item): "store config in the
// environment." The same compiled binary must run in dev, staging, and prod,
// differing only by the environment it's given. That matters because:
//
//   - SECRETS STAY OUT OF GIT. A password committed once is in the history
//     forever, readable by anyone who ever clones the repo.
//   - THE ARTIFACT YOU TEST IS THE ARTIFACT YOU SHIP. If prod needed a
//     different build, your testing proved nothing about prod.
//   - CONTAINERS EXPECT IT. Docker `-e`, compose `environment:`, and
//     Kubernetes ConfigMaps/Secrets all inject config this way. Anything else
//     fights the platform.
//
// WHY A STRUCT INSTEAD OF os.Getenv() SCATTERED AROUND:
// Before this, main.go called os.Getenv in two places and other settings were
// hard-coded literals. That has three problems: you can't see the full set of
// knobs anywhere, typos fail silently at 3am instead of at boot, and defaults
// are duplicated. Loading everything once means the program either starts with
// valid config or refuses to start at all — no half-configured middle ground.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// Env is "development" or "production". It changes DEFAULTS and STRICTNESS,
	// not features — prod refuses insecure fallbacks that dev allows.
	Env  string
	Port string

	DatabaseURL string // empty => in-memory store

	// RedisURL empty => caching disabled entirely. The app MUST work without
	// Redis: a cache is an optimization, never a dependency.
	RedisURL string
	CacheTTL time.Duration

	JWTSecret []byte
	TokenTTL  time.Duration

	LogLevel  slog.Level
	LogFormat string // "json" or "text"

	// ShutdownTimeout bounds how long we wait for in-flight requests to finish
	// before forcing exit. See the graceful-shutdown block in main.go.
	ShutdownTimeout time.Duration
}

func (c Config) IsProduction() bool { return c.Env == "production" }

// Load reads the environment and returns a validated Config, or an error
// explaining exactly what's wrong.
//
// FAIL FAST, AT BOOT. Every problem is caught here, before the server accepts
// a single request. The alternative — discovering a missing secret when the
// first user tries to log in — turns a startup error into a production
// incident.
func Load() (Config, error) {
	cfg := Config{
		Env:             getEnv("APP_ENV", "development"),
		Port:            getEnv("PORT", "8082"),
		DatabaseURL:     getEnv("DATABASE_URL", ""),
		RedisURL:        getEnv("REDIS_URL", ""),
		CacheTTL:        getEnvDuration("CACHE_TTL", 30*time.Second),
		TokenTTL:        getEnvDuration("TOKEN_TTL", 24*time.Hour),
		LogFormat:       getEnv("LOG_FORMAT", ""), // resolved below
		ShutdownTimeout: getEnvDuration("SHUTDOWN_TIMEOUT", 15*time.Second),
	}

	// Defaults that DEPEND on the environment, chosen for who's reading:
	//   dev  -> text logs (human eyes at a terminal), debug level
	//   prod -> JSON logs (a log aggregator parsing fields), info level
	if cfg.LogFormat == "" {
		if cfg.IsProduction() {
			cfg.LogFormat = "json"
		} else {
			cfg.LogFormat = "text"
		}
	}
	defaultLevel := "debug"
	if cfg.IsProduction() {
		defaultLevel = "info"
	}
	lvl, err := parseLevel(getEnv("LOG_LEVEL", defaultLevel))
	if err != nil {
		return Config{}, err
	}
	cfg.LogLevel = lvl

	// ---- JWT secret: the one setting with real teeth ----
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		// In PRODUCTION a missing secret is fatal. Falling back to a known
		// default would let anyone who has read this source file mint a valid
		// token for ANY user — a total authentication bypass. Refusing to boot
		// is the only safe response.
		if cfg.IsProduction() {
			return Config{}, fmt.Errorf("JWT_SECRET is required when APP_ENV=production")
		}
		// In development, convenience wins — but it must be LOUD (main.go warns).
		secret = "dev-only-insecure-secret-change-me"
	} else if cfg.IsProduction() && len(secret) < 32 {
		// HS256 signs with this secret; a short one is brute-forceable offline
		// by anyone holding a single token. 32 bytes is the usual floor.
		return Config{}, fmt.Errorf("JWT_SECRET must be at least 32 characters in production (got %d)", len(secret))
	}
	cfg.JWTSecret = []byte(secret)

	if cfg.ShutdownTimeout <= 0 {
		return Config{}, fmt.Errorf("SHUTDOWN_TIMEOUT must be positive")
	}
	return cfg, nil
}

// UsesInsecureDevSecret reports whether we fell back to the built-in secret,
// so main.go can warn about it. Returning this rather than logging from inside
// config keeps the package free of side effects — it computes a value, it
// doesn't decide how to tell anyone.
func (c Config) UsesInsecureDevSecret() bool {
	return string(c.JWTSecret) == "dev-only-insecure-secret-change-me"
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		// LookupEnv (not Getenv) distinguishes "unset" from "set to empty".
		return v
	}
	return fallback
}

// getEnvDuration accepts Go duration strings: "15s", "24h", "500ms".
// Malformed values fall back to the default rather than crashing — a typo in
// a timeout shouldn't take the service down.
func getEnvDuration(key string, fallback time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}

// getEnvInt is unused today but kept as the obvious next helper (pool sizes,
// rate limits). Delete it if it's still unused by Week 3.
func getEnvInt(key string, fallback int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("invalid LOG_LEVEL %q (want debug|info|warn|error)", s)
}
