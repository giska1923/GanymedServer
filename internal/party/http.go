package party

import (
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/giska1923/GanymedServer/internal/auth"
	"github.com/giska1923/GanymedServer/internal/httpjson"
	"github.com/giska1923/GanymedServer/internal/problem"
	"github.com/giska1923/GanymedServer/internal/realtime"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Register adds the party routes, all behind requireAuth. The HTTP API is the source of truth for
// party state; pushes over the realtime socket only say "re-fetch".
func (s *Service) Register(mux *http.ServeMux, requireAuth func(http.Handler) http.Handler) {
	route := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, requireAuth(h)) }
	route("GET /v1/party", s.handleGet)
	route("POST /v1/party", s.handleCreate)
	route("POST /v1/party/leave", s.handleLeave)
	route("POST /v1/party/kick", s.handleKick)
	route("POST /v1/party/invites", s.handleInvite)
	route("GET /v1/party/invites", s.handleListInvites)
	route("POST /v1/party/invites/{party_id}/accept", s.handleAccept)
	route("POST /v1/party/invites/{party_id}/decline", s.handleDecline)
}

type memberJSON struct {
	AccountID   string          `json:"account_id"`
	DisplayName string          `json:"display_name"`
	Status      realtime.Status `json:"status"`
	Leader      bool            `json:"leader"`
}

type partyJSON struct {
	PartyID  string       `json:"party_id"`
	LeaderID string       `json:"leader_id"`
	Members  []memberJSON `json:"members"`
}

func toJSON(p Party) *partyJSON {
	out := &partyJSON{PartyID: p.ID, LeaderID: p.LeaderID, Members: make([]memberJSON, len(p.Members))}
	for i, m := range p.Members {
		out.Members[i] = memberJSON{m.AccountID, m.DisplayName, m.Status, m.AccountID == p.LeaderID}
	}
	return out
}

// writeParty answers {"party": {...}}, or {"party": null} for a player in no party: a normal
// state, not an error (the same choice as GET /v1/leaderboards/{board}/me).
func writeParty(w http.ResponseWriter, status int, p *partyJSON) {
	httpjson.Write(w, status, struct {
		Party *partyJSON `json:"party"`
	}{p})
}

// fail maps the package's errors onto the contract's problem types. Anything unrecognised is a
// 500, logged with its real cause.
func (s *Service) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotInParty):
		problem.Write(w, http.StatusNotFound, problem.TypeNotFound, err.Error())
	case errors.Is(err, ErrNoInvite):
		problem.Write(w, http.StatusNotFound, problem.TypeNotFound, err.Error())
	case errors.Is(err, ErrNotLeader):
		problem.Write(w, http.StatusForbidden, problem.TypeNotPartyLeader, err.Error())
	case errors.Is(err, ErrAlreadyInParty):
		problem.Write(w, http.StatusConflict, problem.TypeAlreadyInParty, err.Error())
	case errors.Is(err, ErrPartyFull):
		problem.Write(w, http.StatusConflict, problem.TypePartyFull, err.Error())
	case errors.Is(err, ErrInvalidTarget):
		problem.Write(w, http.StatusBadRequest, problem.TypeInvalidRequest, "you cannot target yourself")
	default:
		s.log.Error("party request failed", "err", err)
		problem.Write(w, http.StatusInternalServerError, problem.TypeInternal, "")
	}
}

func caller(r *http.Request) string {
	id, _ := auth.AccountID(r.Context())
	return id
}

func (s *Service) handleGet(w http.ResponseWriter, r *http.Request) {
	p, err := s.Get(r.Context(), caller(r))
	if errors.Is(err, ErrNotInParty) {
		writeParty(w, http.StatusOK, nil)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeParty(w, http.StatusOK, toJSON(p))
}

func (s *Service) handleCreate(w http.ResponseWriter, r *http.Request) {
	p, err := s.Create(r.Context(), caller(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeParty(w, http.StatusCreated, toJSON(p))
}

func (s *Service) handleLeave(w http.ResponseWriter, r *http.Request) {
	if err := s.Leave(r.Context(), caller(r)); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// targetBody decodes {"account_id": "<uuid>"} for invite and kick.
func targetBody(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req struct {
		AccountID string `json:"account_id"`
	}
	if err := httpjson.Decode(w, r, &req); err != nil {
		problem.Write(w, http.StatusBadRequest, problem.TypeInvalidRequest, err.Error())
		return "", false
	}
	if !uuidPattern.MatchString(req.AccountID) {
		problem.Write(w, http.StatusBadRequest, problem.TypeInvalidRequest, "account_id must be an account UUID")
		return "", false
	}
	return req.AccountID, true
}

func (s *Service) handleKick(w http.ResponseWriter, r *http.Request) {
	target, ok := targetBody(w, r)
	if !ok {
		return
	}
	err := s.Kick(r.Context(), caller(r), target)
	if errors.Is(err, ErrNotInParty) {
		// Either the caller has no party or the target is not in it; both are "no such member".
		problem.Write(w, http.StatusNotFound, problem.TypeNotFound, "that player is not in your party")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleInvite(w http.ResponseWriter, r *http.Request) {
	target, ok := targetBody(w, r)
	if !ok {
		return
	}
	err := s.Invite(r.Context(), caller(r), target)
	if errors.Is(err, ErrAlreadyInParty) {
		problem.Write(w, http.StatusConflict, problem.TypeAlreadyInParty, "that player is already in your party")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type inviteJSON struct {
	PartyID   string    `json:"party_id"`
	From      fromJSON  `json:"from"`
	ExpiresAt time.Time `json:"expires_at"`
}

type fromJSON struct {
	AccountID   string `json:"account_id"`
	DisplayName string `json:"display_name"`
}

func (s *Service) handleListInvites(w http.ResponseWriter, r *http.Request) {
	invites, err := s.Invites(r.Context(), caller(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]inviteJSON, len(invites)) // make: an empty list encodes as [], never null
	for i, inv := range invites {
		out[i] = inviteJSON{inv.PartyID, fromJSON{inv.FromID, inv.FromName}, inv.ExpiresAt}
	}
	httpjson.Write(w, http.StatusOK, struct {
		Invites []inviteJSON `json:"invites"`
	}{out})
}

func (s *Service) handleAccept(w http.ResponseWriter, r *http.Request) {
	p, err := s.Accept(r.Context(), caller(r), r.PathValue("party_id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeParty(w, http.StatusOK, toJSON(p))
}

func (s *Service) handleDecline(w http.ResponseWriter, r *http.Request) {
	if err := s.Decline(r.Context(), caller(r), r.PathValue("party_id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
