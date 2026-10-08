package leaderboard

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"

	"github.com/giska1923/GanymedServer/internal/auth"
	"github.com/giska1923/GanymedServer/internal/httpjson"
	"github.com/giska1923/GanymedServer/internal/problem"
)

const (
	defaultLimit = 10
	maxLimit     = 100
)

// The engine generates idempotency keys as random UUIDs, and the column is a uuid, so the format
// is checked here to turn a malformed key into a 400 rather than a database error.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Register adds the leaderboard routes, all behind requireAuth.
func (s *Service) Register(mux *http.ServeMux, requireAuth func(http.Handler) http.Handler) {
	mux.Handle("POST /v1/leaderboards/{board}/scores", requireAuth(http.HandlerFunc(s.handleSubmit)))
	mux.Handle("GET /v1/leaderboards/{board}", requireAuth(http.HandlerFunc(s.handleTop)))
	mux.Handle("GET /v1/leaderboards/{board}/me", requireAuth(http.HandlerFunc(s.handleMine)))
}

type standingResponse struct {
	// Pointers so that "no score yet" serializes as null rather than as a misleading 0.
	Rank *int64 `json:"rank"`
	Best *int64 `json:"best"`
}

func (s *Service) handleSubmit(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if !uuidPattern.MatchString(key) {
		problem.Write(w, http.StatusBadRequest, problem.TypeInvalidRequest,
			"send an Idempotency-Key header holding a UUID; reuse it when retrying this same submission")
		return
	}

	var req struct {
		// int64, so a fractional score ("12.5", or "12.0" from a client that formats integers as
		// floats) fails to decode and becomes a 400. A pointer, so a missing score is not 0.
		Score *int64 `json:"score"`
	}
	if err := httpjson.Decode(w, r, &req); err != nil {
		problem.Write(w, http.StatusBadRequest, problem.TypeInvalidRequest, err.Error())
		return
	}
	if req.Score == nil || *req.Score < 0 || *req.Score > MaxScore {
		problem.Write(w, http.StatusBadRequest, problem.TypeInvalidRequest,
			"score is required: an integer from 0 to 9007199254740991 (2^53-1)")
		return
	}

	id, _ := auth.AccountID(r.Context())
	st, replayed, err := s.Submit(r.Context(), id, r.PathValue("board"), key, *req.Score)
	switch {
	case errors.Is(err, ErrUnknownBoard):
		problem.Write(w, http.StatusNotFound, problem.TypeNotFound, "no such leaderboard")
		return
	case errors.Is(err, ErrKeyReused):
		problem.Write(w, http.StatusUnprocessableEntity, problem.TypeIdempotencyKeyReused, err.Error())
		return
	case err != nil:
		s.log.Error("submit score failed", "err", err)
		problem.Write(w, http.StatusInternalServerError, problem.TypeInternal, "")
		return
	}

	// The same header Stripe uses: lets a client (and a test) see that nothing new happened.
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	httpjson.Write(w, http.StatusOK, standingResponse{Rank: &st.Rank, Best: &st.Best})
}

type entryResponse struct {
	Rank        int64  `json:"rank"`
	AccountID   string `json:"account_id"`
	DisplayName string `json:"display_name"`
	Score       int64  `json:"score"`
}

func (s *Service) handleTop(w http.ResponseWriter, r *http.Request) {
	limit := defaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLimit {
			problem.Write(w, http.StatusBadRequest, problem.TypeInvalidRequest, "limit must be an integer from 1 to 100")
			return
		}
		limit = n
	}

	board := r.PathValue("board")
	entries, err := s.Top(r.Context(), board, limit)
	if errors.Is(err, ErrUnknownBoard) {
		problem.Write(w, http.StatusNotFound, problem.TypeNotFound, "no such leaderboard")
		return
	}
	if err != nil {
		s.log.Error("load leaderboard failed", "err", err)
		problem.Write(w, http.StatusInternalServerError, problem.TypeInternal, "")
		return
	}

	// make, not a nil slice: an empty board must encode as [], never null.
	out := make([]entryResponse, len(entries))
	for i, e := range entries {
		out[i] = entryResponse{e.Rank, e.AccountID, e.DisplayName, e.Score}
	}
	httpjson.Write(w, http.StatusOK, struct {
		Board   string          `json:"board"`
		Entries []entryResponse `json:"entries"`
	}{board, out})
}

func (s *Service) handleMine(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.AccountID(r.Context())
	st, found, err := s.Mine(r.Context(), r.PathValue("board"), id)
	switch {
	case errors.Is(err, ErrUnknownBoard):
		problem.Write(w, http.StatusNotFound, problem.TypeNotFound, "no such leaderboard")
		return
	case err != nil:
		s.log.Error("load standing failed", "err", err)
		problem.Write(w, http.StatusInternalServerError, problem.TypeInternal, "")
		return
	}

	// No score yet is a normal state for a HUD, not an error: 200 with nulls.
	resp := standingResponse{}
	if found {
		resp = standingResponse{Rank: &st.Rank, Best: &st.Best}
	}
	httpjson.Write(w, http.StatusOK, resp)
}
