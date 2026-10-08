package matchmaking

import (
	"errors"
	"net/http"
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
}

type matchJSON struct {
	MatchID string   `json:"match_id"`
	Players []string `json:"players"`
}

type ticketJSON struct {
	TicketID      string     `json:"ticket_id"`
	Mode          string     `json:"mode"`
	State         string     `json:"state"`
	Players       []string   `json:"players"`
	CreatedAt     time.Time  `json:"created_at"`
	FailureReason string     `json:"failure_reason,omitempty"`
	Match         *matchJSON `json:"match"`
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
	return out
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
