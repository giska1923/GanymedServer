package fleet

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/giska1923/GanymedServer/internal/httpjson"
	"github.com/giska1923/GanymedServer/internal/problem"
)

// CommandWait is how long a commands long-poll is held before answering 204.
const CommandWait = 20 * time.Second

// Register adds the agent routes. They authenticate with the shared agent secret, not a player's
// access token: agents are infrastructure, not accounts.
func (s *Service) Register(mux *http.ServeMux) {
	mux.Handle("POST /v1/fleet/agents/{agent_id}/heartbeat", s.requireAgent(s.handleHeartbeat))
	mux.Handle("GET /v1/fleet/agents/{agent_id}/commands", s.requireAgent(s.handleCommands))
	mux.Handle("POST /v1/fleet/agents/{agent_id}/servers/{server_id}/allocated", s.requireAgent(s.handleAllocated))
}

// requireAgent checks Authorization: Bearer <agent secret> in constant time.
//
// A plain == on secrets returns as soon as the first differing byte is found, so the response
// time leaks how many leading bytes were right, and an attacker can recover a secret byte by byte
// from timings. subtle.ConstantTimeCompare always looks at every byte. (It does return early on a
// length mismatch, which leaks only the length.)
func (s *Service) requireAgent(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare([]byte(token), s.secret) != 1 {
			problem.Write(w, http.StatusUnauthorized, problem.TypeUnauthorized, "")
			return
		}
		h(w, r)
	})
}

func (s *Service) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Servers []ReportedServer `json:"servers"`
	}
	if err := httpjson.Decode(w, r, &req); err != nil {
		problem.Write(w, http.StatusBadRequest, problem.TypeInvalidRequest, err.Error())
		return
	}
	pub, retire, err := s.Heartbeat(r.Context(), r.PathValue("agent_id"), req.Servers)
	if err != nil {
		s.log.Error("heartbeat failed", "err", err)
		problem.Write(w, http.StatusInternalServerError, problem.TypeInternal, "")
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"connect_token_public_key": pub, "retire": retire})
}

func (s *Service) handleCommands(w http.ResponseWriter, r *http.Request) {
	// The server's WriteTimeout (15 s, internal/server) would cut this 20 s long-poll off
	// mid-wait. The same net/http fact as B3's WebSockets: request timeouts are connection
	// deadlines, and a handler that legitimately runs long must extend its own.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(CommandWait + 10*time.Second)); err != nil {
		s.log.Error("extend write deadline", "err", err)
	}
	cmd, err := s.NextCommand(r.Context(), r.PathValue("agent_id"), CommandWait)
	if err != nil {
		if r.Context().Err() == nil {
			s.log.Error("command long-poll failed", "err", err)
			problem.Write(w, http.StatusInternalServerError, problem.TypeInternal, "")
		}
		return
	}
	if cmd == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	httpjson.Write(w, http.StatusOK, cmd)
}

func (s *Service) handleAllocated(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MatchID string `json:"match_id"`
	}
	if err := httpjson.Decode(w, r, &req); err != nil {
		problem.Write(w, http.StatusBadRequest, problem.TypeInvalidRequest, err.Error())
		return
	}
	err := s.Acknowledge(r.Context(), r.PathValue("agent_id"), r.PathValue("server_id"), req.MatchID)
	switch {
	case errors.Is(err, ErrWithdrawn):
		problem.Write(w, http.StatusConflict, problem.TypeAllocationWithdrawn, "shut this server down")
	case err != nil:
		s.log.Error("acknowledge failed", "err", err)
		problem.Write(w, http.StatusInternalServerError, problem.TypeInternal, "")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
