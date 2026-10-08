// Package leaderboard ranks best scores per board, with idempotent submission.
//
// It owns the leaderboards, score_submissions, best_scores and score_idempotency_keys tables.
// Display names come from the profile module through the Names interface; this package never
// reads the profiles table.
package leaderboard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxScore is 2^53-1, the largest integer a double holds exactly. Scores reach the backend from
// Lua, where every number is a double, through JSON, which most parsers also read as a double.
// Above this, two different scores can arrive as the same number.
const MaxScore = 1<<53 - 1

// KeyRetention is how long an idempotency key is remembered. A retry later than this is treated
// as a new submission; 24 hours is the window Stripe documents, and far longer than any client
// retry loop.
const KeyRetention = 24 * time.Hour

var (
	ErrUnknownBoard = errors.New("no such leaderboard")
	// ErrKeyReused means the Idempotency-Key was already used for a different request. That is
	// a client bug (a key must identify exactly one logical request), so it is refused rather than
	// guessed at.
	ErrKeyReused = errors.New("idempotency key was already used for a different request")
)

// Names resolves display names. Declared here, by the consumer, with the one method this package
// needs; profile.Service satisfies it without knowing it does. This interface is the module
// rule in code: names come from asking the profile module, never from joining its table.
type Names interface {
	DisplayNames(ctx context.Context, accountIDs []string) (map[string]string, error)
}

type Service struct {
	pool  *pgxpool.Pool
	names Names
	log   *slog.Logger
}

func NewService(pool *pgxpool.Pool, names Names, log *slog.Logger) *Service {
	return &Service{pool: pool, names: names, log: log}
}

// Standing is a player's place on a board: the rank of their best score, and that score.
type Standing struct {
	Rank int64
	Best int64
}

type Entry struct {
	Rank        int64
	AccountID   string
	DisplayName string
	Score       int64
}

// Submit records a score under an idempotency key and returns the player's standing.
//
// replayed is true when the key had already been used for this same request: nothing was
// written, and the standing is the one returned the first time, not the current one. That is
// what idempotency means: the same request gets the same response.
//
// How duplicates are handled, including concurrent ones, comes down to the first statement. It
// inserts the key with ON CONFLICT DO NOTHING:
//
//   - New key: the insert succeeds, and this transaction owns the key until it commits.
//   - Key committed earlier: the insert does nothing; read the stored request and response.
//   - Key being inserted right now by another transaction: Postgres makes this insert WAIT for
//     that transaction to finish. If it commits, this is the previous case. If it rolls back,
//     this insert succeeds and this request does the work instead.
//
// So two identical requests racing each other serialize on the key's unique index, and exactly
// one does the work. No in-memory lock, which would not survive two replicas.
func (s *Service) Submit(ctx context.Context, accountID, board, key string, score int64) (standing Standing, replayed bool, err error) {
	if err := s.requireBoard(ctx, board); err != nil {
		return Standing{}, false, err
	}

	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`INSERT INTO score_idempotency_keys (account_id, key, board, score)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (account_id, key) DO NOTHING`,
			accountID, key, board, score)
		if err != nil {
			return fmt.Errorf("claim idempotency key: %w", err)
		}

		if tag.RowsAffected() == 0 {
			var storedBoard string
			var storedScore int64
			err := tx.QueryRow(ctx,
				`SELECT board, score, response_rank, response_best
				 FROM score_idempotency_keys WHERE account_id = $1 AND key = $2`,
				accountID, key).Scan(&storedBoard, &storedScore, &standing.Rank, &standing.Best)
			if err != nil {
				return fmt.Errorf("load idempotency key: %w", err)
			}
			if storedBoard != board || storedScore != score {
				return ErrKeyReused
			}
			replayed = true
			return nil
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO score_submissions (board, account_id, score) VALUES ($1, $2, $3)`,
			board, accountID, score); err != nil {
			return fmt.Errorf("log submission: %w", err)
		}

		// The projection. The WHERE on DO UPDATE makes a lower score a no-op: the existing row is
		// kept, achieved_at included, so "who reached this score first" stays true.
		if _, err := tx.Exec(ctx,
			`INSERT INTO best_scores (board, account_id, score, achieved_at)
			 VALUES ($1, $2, $3, now())
			 ON CONFLICT (board, account_id) DO UPDATE
			   SET score = excluded.score, achieved_at = excluded.achieved_at
			   WHERE excluded.score > best_scores.score`,
			board, accountID, score); err != nil {
			return fmt.Errorf("update best score: %w", err)
		}

		standing, err = standingOf(ctx, tx, board, accountID)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx,
			`UPDATE score_idempotency_keys SET response_rank = $3, response_best = $4
			 WHERE account_id = $1 AND key = $2`,
			accountID, key, standing.Rank, standing.Best)
		if err != nil {
			return fmt.Errorf("store idempotent response: %w", err)
		}
		return nil
	})
	if err != nil {
		return Standing{}, false, err
	}
	return standing, replayed, nil
}

// Top returns the first limit entries, best first.
//
// Ranks are standard competition ranking ("1224"): tied scores share a rank, and the next rank
// skips. Tied players are listed in the order they reached the score. rank() is a window
// function; with the index delivering rows already in (score DESC, achieved_at) order, Postgres
// computes it while streaming and the LIMIT stops the scan early. See docs/backend/leaderboard.md
// for the measured plan.
func (s *Service) Top(ctx context.Context, board string, limit int) ([]Entry, error) {
	if err := s.requireBoard(ctx, board); err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx,
		`SELECT rank() OVER (ORDER BY score DESC), account_id::text, score
		 FROM best_scores
		 WHERE board = $1
		 ORDER BY score DESC, achieved_at
		 LIMIT $2`,
		board, limit)
	if err != nil {
		return nil, fmt.Errorf("load leaderboard: %w", err)
	}
	entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Entry, error) {
		var e Entry
		err := row.Scan(&e.Rank, &e.AccountID, &e.Score)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("load leaderboard: %w", err)
	}

	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.AccountID
	}
	names, err := s.names.DisplayNames(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range entries {
		entries[i].DisplayName = names[entries[i].AccountID]
	}
	return entries, nil
}

// Mine returns the player's standing, or found == false if they have no score on the board.
func (s *Service) Mine(ctx context.Context, board, accountID string) (standing Standing, found bool, err error) {
	if err := s.requireBoard(ctx, board); err != nil {
		return Standing{}, false, err
	}
	standing, err = standingOf(ctx, s.pool, board, accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Standing{}, false, nil
	}
	if err != nil {
		return Standing{}, false, err
	}
	return standing, true, nil
}

// querier is what standingOf needs: satisfied by both *pgxpool.Pool and pgx.Tx, so the same
// query runs inside Submit's transaction and outside it in Mine.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// standingOf ranks a player's best score as 1 + the number of strictly better scores. That is an
// index-only count over every row above the player: O(rank), so worst for the last place. This is
// the query B2 measured at 100k and 1M rows.
func standingOf(ctx context.Context, q querier, board, accountID string) (Standing, error) {
	var st Standing
	err := q.QueryRow(ctx,
		`SELECT b.score,
		        (SELECT count(*) + 1 FROM best_scores o WHERE o.board = b.board AND o.score > b.score)
		 FROM best_scores b
		 WHERE b.board = $1 AND b.account_id = $2`,
		board, accountID).Scan(&st.Best, &st.Rank)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Standing{}, err
		}
		return Standing{}, fmt.Errorf("rank player: %w", err)
	}
	return st, nil
}

func (s *Service) requireBoard(ctx context.Context, board string) error {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM leaderboards WHERE id = $1)`, board).Scan(&exists); err != nil {
		return fmt.Errorf("look up leaderboard: %w", err)
	}
	if !exists {
		return ErrUnknownBoard
	}
	return nil
}

// ExpireKeys deletes idempotency keys older than KeyRetention, every interval, until ctx is
// cancelled.
//
// A background goroutine in a request-driven server, so its lifetime is the thing to get right:
// it owns no state, exits promptly when ctx is cancelled, and main waits for it before closing
// the pool it uses. With two replicas both run it, which is harmless: DELETE of already-deleted
// rows does nothing.
func (s *Service) ExpireKeys(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := s.expireOnce(ctx)
			if err != nil {
				if ctx.Err() == nil {
					s.log.Error("expire idempotency keys failed", "err", err)
				}
				continue
			}
			if n > 0 {
				s.log.Info("expired idempotency keys", "count", n)
			}
		}
	}
}

// expireOnce is one pass of ExpireKeys, separate so a test can run it without waiting.
func (s *Service) expireOnce(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM score_idempotency_keys WHERE created_at < now() - $1::bigint * interval '1 second'`,
		int64(KeyRetention/time.Second))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
