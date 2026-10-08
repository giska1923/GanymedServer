// Package redistest gives a test its own empty Redis database.
//
// Redis has no schemas; it has 16 numbered databases (0–15). A test claims a free one with a lock
// key in database 0 (SET NX, with a TTL so a crashed test cannot hold it forever), flushes it,
// and releases it at cleanup. Packages run their tests in parallel processes, so the lock, not an
// in-process counter, is what keeps two tests out of the same database.
//
// One trap this does not cover, by design: **pub/sub ignores database numbers.** A channel is
// global to the server. Tests stay apart on channels only because every account ID they use is
// a fresh random UUID.
package redistest

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

const lockTTL = 10 * time.Minute

// New returns a client on a freshly flushed database, or skips the test when TEST_REDIS_URL is
// unset, so `go test ./...` still passes on a machine with no Redis.
func New(t testing.TB) *redis.Client {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set; skipping Redis test")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse TEST_REDIS_URL: %v", err)
	}
	ctx := context.Background()

	admin := redis.NewClient(&redis.Options{Addr: opts.Addr, Password: opts.Password, DB: 0})
	owner := fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())

	db := 0
	for deadline := time.Now().Add(30 * time.Second); db == 0; {
		for n := 1; n <= 15; n++ {
			ok, err := admin.SetNX(ctx, fmt.Sprintf("redistest:lock:%d", n), owner, lockTTL).Result()
			if err != nil {
				admin.Close()
				t.Fatalf("claim test database: %v", err)
			}
			if ok {
				db = n
				break
			}
		}
		if db == 0 {
			if time.Now().After(deadline) {
				admin.Close()
				t.Fatal("no free Redis test database (1-15) within 30s")
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	opts.DB = db
	rdb := redis.NewClient(opts)
	if err := rdb.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flush test database %d: %v", db, err)
	}

	t.Cleanup(func() {
		rdb.Close()
		admin.Del(context.Background(), fmt.Sprintf("redistest:lock:%d", db))
		admin.Close()
	})
	return rdb
}
