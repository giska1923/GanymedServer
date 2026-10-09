package matchmaking

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/giska1923/GanymedServer/internal/auth"
	"github.com/giska1923/GanymedServer/internal/httpjson"
	"github.com/giska1923/GanymedServer/internal/problem"
)

// Register adds the matchmaking routes, all behind requireAuth.
func (s *Service) Register(mux *http.ServeMux, requireAuth func(http.Handler) http.Handler) {
	route := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, requireAuth(h)) }
	route("POST /v1/matchmaking/tickets", s.handleEnqueue)
	route("GET /v1/matchmaking/ticket", s.handleCurrent)
	route("GET /v1/matchmaking/tickets/{ticket_id}", s.handleGet)
	route("DELETE /v1/matchmaking/tickets/{ticket_id}", s.handleCancel)
	// Called by game servers, not players: authenticated by the per-match result token instead.
	mux.HandleFunc("POST /v1/matches/{match_id}/result", s.handleResult)
}

type matchJSON struct {
	MatchID string   `json:"match_id"`
	Players []string `json:"players"`
}

type serverJSON struct {
	Address      string `json:"address"`
	ConnectToken string `json:"connect_token"`
}

type resultJSON struct {
	Outcome      string `json:"outcome"`
	RatingChange int    `json:"rating_change"`
}

type ticketJSON struct {
	TicketID      string      `json:"ticket_id"`
	Mode          string      `json:"mode"`
	State         string      `json:"state"`
	Players       []string    `json:"players"`
	CreatedAt     time.Time   `json:"created_at"`
	FailureReason string      `json:"failure_reason,omitempty"`
	Match         *matchJSON  `json:"match"`
	Server        *serverJSON `json:"server"`
	Result        *resultJSON `json:"result"`
}

func toJSON(v TicketView) ticketJSON {
	out := ticketJSON{TicketID: v.ID, Mode: v.Mode, State: v.State, Players: v.Players,
		CreatedAt: v.Created, FailureReason: v.Reason}
	if v.MatchID != "" {
		out.Match = &matchJSON{MatchID: v.MatchID, Players: v.MatchPlayers}
		if out.Match.Players == nil {
			out.Match.Players = []string{}
		}
	}
	if v.ConnectToken != "" {
		out.Server = &serverJSON{Address: v.ServerAddr, ConnectToken: v.ConnectToken}
	}
	if v.Outcome != "" {
		out.Result = &resultJSON{Outcome: v.Outcome, RatingChange: v.RatingChange}
	}
	return out
}

func (s *Service) handleResult(w http.ResponseWriter, r *http.Request) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		problem.Write(w, http.StatusUnauthorized, problem.TypeUnauthorized, "send Authorization: Bearer <result_token>")
		return
	}
	var req struct {
		Outcome string `json:"outcome"`
	}
	if err := httpjson.Decode(w, r, &req); err != nil {
		problem.Write(w, http.StatusBadRequest, problem.TypeInvalidRequest, err.Error())
		return
	}
	if req.Outcome != "victory" && req.Outcome != "defeat" {
		problem.Write(w, http.StatusBadRequest, problem.TypeInvalidRequest, `outcome must be "victory" or "defeat"`)
		return
	}
	res, err := s.ReportResult(r.Context(), r.PathValue("match_id"), token, req.Outcome)
	switch {
	case errors.Is(err, ErrUnauthorized):
		problem.Write(w, http.StatusUnauthorized, problem.TypeUnauthorized, "")
	case errors.Is(err, ErrResultConflict):
		problem.Write(w, http.StatusConflict, problem.TypeResultConflict, "")
	case err != nil:
		s.log.Error("record result failed", "err", err)
		problem.Write(w, http.StatusInternalServerError, problem.TypeInternal, "")
	default:
		httpjson.Write(w, http.StatusOK, map[string]any{
			"match_id": res.MatchID, "outcome": res.Outcome, "rating_change": res.RatingChange,
		})
	}
}

func caller(r *http.Request) string {
	id, _ := auth.AccountID(r.Context())
	return id
}

func (s *Service) fail(w http.ResponseWriter, err error) {
	var notQueued ErrNotQueued
	switch {
	case errors.Is(err, ErrUnknownMode):
		problem.Write(w, http.StatusBadRequest, problem.TypeInvalidRequest, "unknown mode; available: coop")
	case errors.Is(err, ErrPartyTooLarge):
		problem.Write(w, http.StatusBadRequest, problem.TypeInvalidRequest, err.Error())
	case errors.Is(err, ErrNotLeader):
		problem.Write(w, http.StatusForbidden, problem.TypeNotPartyLeader, err.Error())
	case errors.Is(err, ErrAlreadyQueued):
		problem.Write(w, http.StatusConflict, problem.TypeAlreadyQueued, err.Error())
	case errors.Is(err, ErrNoTicket):
		problem.Write(w, http.StatusNotFound, problem.TypeNotFound, "no such ticket")
	case errors.As(err, &notQueued):
		problem.Write(w, http.StatusConflict, problem.TypeTicketNotQueued, "the ticket is "+notQueued.State)
	default:
		s.log.Error("matchmaking request failed", "err", err)
		problem.Write(w, http.StatusInternalServerError, problem.TypeInternal, "")
	}
}

func (s *Service) handleEnqueue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string `json:"mode"`
	}
	if err := httpjson.Decode(w, r, &req); err != nil {
		problem.Write(w, http.StatusBadRequest, problem.TypeInvalidRequest, err.Error())
		return
	}
	v, err := s.Enqueue(r.Context(), caller(r), req.Mode)
	if err != nil {
		s.fail(w, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, toJSON(v))
}

func (s *Service) handleCurrent(w http.ResponseWriter, r *http.Request) {
	v, err := s.Current(r.Context(), caller(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	var t *ticketJSON
	if v != nil {
		j := toJSON(*v)
		t = &j
	}
	httpjson.Write(w, http.StatusOK, struct {
		Ticket *ticketJSON `json:"ticket"`
	}{t})
}

func (s *Service) handleGet(w http.ResponseWriter, r *http.Request) {
	v, err := s.Ticket(r.Context(), caller(r), r.PathValue("ticket_id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, toJSON(v))
}

func (s *Service) handleCancel(w http.ResponseWriter, r *http.Request) {
	v, err := s.Cancel(r.Context(), caller(r), r.PathValue("ticket_id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, toJSON(v))
}
