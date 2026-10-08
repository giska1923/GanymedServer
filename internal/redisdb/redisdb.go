// Package redisdb opens the Redis client. Like internal/db for Postgres, it knows nothing about
// any module's keys: each module owns its own key prefixes and channels (see docs/backend/db.md).
package redisdb

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// Open connects and pings once, so a wrong URL fails startup instead of the first request.
// The returned *redis.Client is a connection pool, safe for concurrent use, like *pgxpool.Pool.
func Open(ctx context.Context, url string) (*redis.Client, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		// Not wrapped with the URL: it may carry a password.
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	rdb := redis.NewClient(opts)
	if err := rdb.Ping(ctx).Err(); err != nil {
		rdb.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return rdb, nil
}
