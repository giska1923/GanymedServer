// Package config turns environment variables into a typed, validated Config.
//
// It is parsed once at startup. A missing or malformed required value is a startup error,
// reported with every problem at once, so a misconfigured process never starts and then
// fails on first use.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

type Config struct {
	HTTPAddr        string        // GS_HTTP_ADDR, default ":8080"
	DatabaseURL     string        // GS_DATABASE_URL, required
	RedisURL        string        // GS_REDIS_URL, required (redis://host:port/db)
	ReplicaID       string        // GS_REPLICA_ID, default the hostname; names this process in presence and logs
	JWTSecret       []byte        // GS_JWT_SECRET, required, at least 32 bytes
	ConnectTokenKey string        // GS_CONNECT_TOKEN_KEY, required: base64 Ed25519 seed (32 bytes) signing connect tokens
	AgentSecret     []byte        // GS_FLEET_AGENT_SECRET, required, at least 32 bytes: fleet agents authenticate with it
	AccessTokenTTL  time.Duration // GS_ACCESS_TOKEN_TTL, default 15m
	RefreshTokenTTL time.Duration // GS_REFRESH_TOKEN_TTL, default 720h (30 days)
	ShutdownTimeout time.Duration // GS_SHUTDOWN_TIMEOUT, default 20s
	LogLevel        slog.Level    // GS_LOG_LEVEL: debug, info, warn, error; default info
}

// MinJWTSecretLen is the floor for the HMAC key. HS256 with a key shorter than its 32-byte
// output is weaker than the algorithm, and a short key is almost always a typed-in password.
const MinJWTSecretLen = 32

// Load reads the environment through getenv, which is os.Getenv in production and a map in
// tests. Every problem found is returned, joined, rather than only the first.
func Load(getenv func(string) string) (Config, error) {
	var errs []error

	c := Config{
		HTTPAddr:    stringOr(getenv("GS_HTTP_ADDR"), ":8080"),
		DatabaseURL: getenv("GS_DATABASE_URL"),
		RedisURL:    getenv("GS_REDIS_URL"),
		ReplicaID:   getenv("GS_REPLICA_ID"),
		JWTSecret:   []byte(getenv("GS_JWT_SECRET")),
		// Parsed by connecttoken.ParsePrivateKey in main; here only presence is checked, so the
		// config package does not depend on the token package.
		ConnectTokenKey: getenv("GS_CONNECT_TOKEN_KEY"),
		AgentSecret:     []byte(getenv("GS_FLEET_AGENT_SECRET")),
	}
	if c.ConnectTokenKey == "" {
		errs = append(errs, errors.New("GS_CONNECT_TOKEN_KEY is required (a base64 Ed25519 seed: openssl rand -base64 32)"))
	}
	if len(c.AgentSecret) < MinJWTSecretLen {
		errs = append(errs, fmt.Errorf("GS_FLEET_AGENT_SECRET must be at least %d bytes (got %d)", MinJWTSecretLen, len(c.AgentSecret)))
	}

	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("GS_DATABASE_URL is required"))
	}
	if c.RedisURL == "" {
		errs = append(errs, errors.New("GS_REDIS_URL is required"))
	}
	if c.ReplicaID == "" {
		// In Compose the hostname is the container ID, unique per replica, which is all this needs.
		host, err := os.Hostname()
		if err != nil {
			errs = append(errs, fmt.Errorf("GS_REPLICA_ID unset and no hostname: %w", err))
		}
		c.ReplicaID = host
	}
	if len(c.JWTSecret) < MinJWTSecretLen {
		errs = append(errs, fmt.Errorf("GS_JWT_SECRET must be at least %d bytes (got %d)", MinJWTSecretLen, len(c.JWTSecret)))
	}

	c.AccessTokenTTL = duration(getenv, "GS_ACCESS_TOKEN_TTL", 15*time.Minute, &errs)
	c.RefreshTokenTTL = duration(getenv, "GS_REFRESH_TOKEN_TTL", 30*24*time.Hour, &errs)
	c.ShutdownTimeout = duration(getenv, "GS_SHUTDOWN_TIMEOUT", 20*time.Second, &errs)

	if err := c.LogLevel.UnmarshalText([]byte(stringOr(getenv("GS_LOG_LEVEL"), "info"))); err != nil {
		errs = append(errs, fmt.Errorf("GS_LOG_LEVEL: %w", err))
	}

	if c.AccessTokenTTL >= c.RefreshTokenTTL {
		errs = append(errs, errors.New("GS_ACCESS_TOKEN_TTL must be shorter than GS_REFRESH_TOKEN_TTL"))
	}

	return c, errors.Join(errs...)
}

// FromEnv is Load over the process environment.
func FromEnv() (Config, error) { return Load(os.Getenv) }

func stringOr(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func duration(getenv func(string) string, key string, fallback time.Duration, errs *[]error) time.Duration {
	v := getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		*errs = append(*errs, fmt.Errorf("%s: want a positive duration like 15m, got %q", key, v))
		return fallback
	}
	return d
}
