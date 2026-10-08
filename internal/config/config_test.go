package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{
		"GS_DATABASE_URL": "postgres://x",
		"GS_REDIS_URL":    "redis://x",
		"GS_JWT_SECRET":   strings.Repeat("s", 32),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ReplicaID == "" {
		t.Error("ReplicaID should default to the hostname")
	}
	if c.HTTPAddr != ":8080" || c.AccessTokenTTL != 15*time.Minute || c.RefreshTokenTTL != 720*time.Hour {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

// Every problem is reported at once, not only the first.
func TestLoadReportsAllProblems(t *testing.T) {
	_, err := Load(env(map[string]string{
		"GS_JWT_SECRET":       "short",
		"GS_ACCESS_TOKEN_TTL": "soon",
		"GS_LOG_LEVEL":        "loud",
	}))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"GS_DATABASE_URL", "GS_REDIS_URL", "GS_JWT_SECRET", "GS_ACCESS_TOKEN_TTL", "GS_LOG_LEVEL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%v", want, err)
		}
	}
}

func TestLoadRejectsAccessLongerThanRefresh(t *testing.T) {
	_, err := Load(env(map[string]string{
		"GS_DATABASE_URL":      "postgres://x",
		"GS_REDIS_URL":         "redis://x",
		"GS_JWT_SECRET":        strings.Repeat("s", 32),
		"GS_ACCESS_TOKEN_TTL":  "2h",
		"GS_REFRESH_TOKEN_TTL": "1h",
	}))
	if err == nil {
		t.Fatal("expected an error")
	}
}
