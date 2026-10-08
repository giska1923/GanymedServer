package leaderboard

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

// BenchmarkLeaderboard is B2's measurement: is Postgres fast enough for ranks, or is there a
// measured case for Redis? It seeds a board with 100k and then 1M best scores and times the two
// read paths through the service (so including the board check and, for Top, the name lookup):
//
//   - top10: the first page, an index scan the LIMIT stops early;
//   - rank-first / rank-median / rank-last: Mine for a player at each position. The rank is a
//     count of strictly better scores, so its cost grows with the number of rows above the
//     player, and rank-last is the worst case.
//
// Seeding takes several seconds, so it runs only when asked:
//
//	GS_BENCH=1 go test -run '^$' -bench Leaderboard -benchtime 2s ./internal/leaderboard/
func BenchmarkLeaderboard(b *testing.B) {
	if os.Getenv("GS_BENCH") == "" {
		b.Skip("set GS_BENCH=1 to seed 1.1M rows and run the leaderboard benchmark")
	}
	ctx := context.Background()

	for _, n := range []int{100_000, 1_000_000} {
		s := newTestService(b)
		first, median, last := seedBench(b, s, n)
		explain(b, s, n, last)

		b.Run(fmt.Sprintf("%dk/top10", n/1000), func(b *testing.B) {
			for b.Loop() {
				if _, err := s.Top(ctx, "bench", 10); err != nil {
					b.Fatal(err)
				}
			}
		})
		for _, p := range []struct{ name, id string }{{"rank-first", first}, {"rank-median", median}, {"rank-last", last}} {
			b.Run(fmt.Sprintf("%dk/%s", n/1000, p.name), func(b *testing.B) {
				for b.Loop() {
					if _, _, err := s.Mine(ctx, "bench", p.id); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// seedBench fills board "bench" with n random scores in [0, 1e9), plus three known players at the
// top, the middle and the bottom. VACUUM ANALYZE afterwards matters: an index-only scan can skip
// the table only for pages the visibility map marks all-visible, and only VACUUM sets that. Without
// it, the "index-only" count would visit the heap for every row and measure the wrong thing.
func seedBench(b *testing.B, s *Service, n int) (first, median, last string) {
	b.Helper()
	ctx := context.Background()
	first, median, last = newID(), newID(), newID()
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO leaderboards (id) VALUES ('bench')`, nil},
		{`INSERT INTO best_scores (board, account_id, score, achieved_at)
		  SELECT 'bench', gen_random_uuid(), (random() * 1e9)::bigint, now() - random() * interval '30 days'
		  FROM generate_series(1, $1)`, []any{n}},
		{`INSERT INTO best_scores (board, account_id, score, achieved_at) VALUES
		  ('bench', $1, 2000000000, now()), ('bench', $2, 500000000, now()), ('bench', $3, -1, now())`,
			[]any{first, median, last}},
		{`VACUUM ANALYZE best_scores`, nil},
	} {
		if _, err := s.pool.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			b.Fatal(err)
		}
	}
	return first, median, last
}

// explain logs the plans and server-side timings of the two queries, so the numbers come with
// their reason: which index, how many rows, how many heap fetches.
func explain(b *testing.B, s *Service, n int, last string) {
	b.Helper()
	ctx := context.Background()
	// EXPLAIN accepts bind parameters like any other statement, so even here nothing is spliced
	// into the SQL text.
	for _, q := range []struct {
		name, sql string
		args      []any
	}{
		{"top10", `SELECT rank() OVER (ORDER BY score DESC), account_id::text, score
		           FROM best_scores WHERE board = 'bench' ORDER BY score DESC, achieved_at LIMIT 10`, nil},
		{"rank-last", `SELECT b.score,
		           (SELECT count(*) + 1 FROM best_scores o WHERE o.board = b.board AND o.score > b.score)
		           FROM best_scores b WHERE b.board = 'bench' AND b.account_id = $1`, []any{last}},
	} {
		rows, err := s.pool.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) "+q.sql, q.args...)
		if err != nil {
			b.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				b.Fatal(err)
			}
			plan = append(plan, line)
		}
		rows.Close()
		b.Logf("EXPLAIN %s at %d rows:\n%s", q.name, n, strings.Join(plan, "\n"))
	}
}
