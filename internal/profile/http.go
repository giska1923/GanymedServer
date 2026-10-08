package profile

import (
	"errors"
	"net/http"

	"github.com/giska1923/GanymedServer/internal/auth"
	"github.com/giska1923/GanymedServer/internal/httpjson"
	"github.com/giska1923/GanymedServer/internal/problem"
)

// Register adds the profile routes. requireAuth is auth's middleware in the standard Go
// middleware shape, func(http.Handler) http.Handler, so this package depends on what it does,
// not on auth.Service.
func (s *Service) Register(mux *http.ServeMux, requireAuth func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/me/profile", requireAuth(http.HandlerFunc(s.handleGet)))
	mux.Handle("PATCH /v1/me/profile", requireAuth(http.HandlerFunc(s.handlePatch)))
}

type profileResponse struct {
	AccountID   string `json:"account_id"`
	DisplayName string `json:"display_name"`
	Rating      int    `json:"rating"`
}

func (s *Service) handleGet(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.AccountID(r.Context())
	p, err := s.Get(r.Context(), id)
	if err != nil {
		s.log.Error("load profile failed", "err", err)
		problem.Write(w, http.StatusInternalServerError, problem.TypeInternal, "")
		return
	}
	httpjson.Write(w, http.StatusOK, profileResponse{p.AccountID, p.DisplayName, p.Rating})
}

func (s *Service) handlePatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		// A pointer distinguishes "absent" from "empty": PATCH changes only what is sent. With
		// one field today, absent means there is nothing to change, which is a client mistake.
		DisplayName *string `json:"display_name"`
	}
	if err := httpjson.Decode(w, r, &req); err != nil {
		problem.Write(w, http.StatusBadRequest, problem.TypeInvalidRequest, err.Error())
		return
	}
	if req.DisplayName == nil {
		problem.Write(w, http.StatusBadRequest, problem.TypeInvalidRequest, "display_name is required")
		return
	}

	id, _ := auth.AccountID(r.Context())
	p, err := s.SetDisplayName(r.Context(), id, *req.DisplayName)
	if errors.Is(err, ErrInvalidDisplayName) {
		problem.Write(w, http.StatusBadRequest, problem.TypeInvalidRequest, err.Error())
		return
	}
	if err != nil {
		s.log.Error("set display name failed", "err", err)
		problem.Write(w, http.StatusInternalServerError, problem.TypeInternal, "")
		return
	}
	httpjson.Write(w, http.StatusOK, profileResponse{p.AccountID, p.DisplayName, p.Rating})
}
