package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/giska1923/GanymedServer/internal/httpjson"
	"github.com/giska1923/GanymedServer/internal/problem"
)

// Device IDs must look like what a client generates: a random UUID is 36 characters. The floor is
// an entropy floor, since the device ID is the credential; the ceiling just bounds the input.
const (
	minDeviceIDLen = 32
	maxDeviceIDLen = 128
)

type accountIDKey struct{}

// AccountID returns the authenticated account for a request that passed RequireAuth.
func AccountID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(accountIDKey{}).(string)
	return id, ok
}

// Register adds the auth routes to mux.
func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/auth/device", s.handleDeviceLogin)
	mux.HandleFunc("POST /v1/auth/refresh", s.handleRefresh)
	mux.Handle("GET /v1/me", s.RequireAuth(http.HandlerFunc(s.handleMe)))
}

type sessionResponse struct {
	AccountID    string `json:"account_id"`
	TokenType    string `json:"token_type"`
	AccessToken  string `json:"access_token"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

func writeSession(w http.ResponseWriter, sess Session) {
	// RFC 6749 §5.1: a response carrying tokens must not be cached by anything in between.
	w.Header().Set("Cache-Control", "no-store")
	httpjson.Write(w, http.StatusOK, sessionResponse{
		AccountID:    sess.AccountID,
		TokenType:    "Bearer",
		AccessToken:  sess.AccessToken,
		ExpiresIn:    int64(sess.AccessTokenTTL / time.Second),
		RefreshToken: sess.RefreshToken,
	})
}

func (s *Service) handleDeviceLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceID string `json:"device_id"`
	}
	if err := httpjson.Decode(w, r, &req); err != nil {
		problem.Write(w, http.StatusBadRequest, problem.TypeInvalidRequest, err.Error())
		return
	}
	if n := len(req.DeviceID); n < minDeviceIDLen || n > maxDeviceIDLen {
		problem.Write(w, http.StatusBadRequest, problem.TypeInvalidRequest,
			"device_id must be 32 to 128 characters of random data, such as a UUID")
		return
	}

	sess, err := s.LoginDevice(r.Context(), req.DeviceID)
	if err != nil {
		s.log.Error("device login failed", "err", err)
		problem.Write(w, http.StatusInternalServerError, problem.TypeInternal, "")
		return
	}
	writeSession(w, sess)
}

func (s *Service) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := httpjson.Decode(w, r, &req); err != nil {
		problem.Write(w, http.StatusBadRequest, problem.TypeInvalidRequest, err.Error())
		return
	}
	if req.RefreshToken == "" {
		problem.Write(w, http.StatusBadRequest, problem.TypeInvalidRequest, "refresh_token is required")
		return
	}

	sess, err := s.Refresh(r.Context(), req.RefreshToken)
	if errors.Is(err, ErrInvalidRefreshToken) {
		problem.Write(w, http.StatusUnauthorized, problem.TypeInvalidRefreshToken, "log in again")
		return
	}
	if err != nil {
		s.log.Error("refresh failed", "err", err)
		problem.Write(w, http.StatusInternalServerError, problem.TypeInternal, "")
		return
	}
	writeSession(w, sess)
}

func (s *Service) handleMe(w http.ResponseWriter, r *http.Request) {
	id, _ := AccountID(r.Context())
	acct, err := s.Account(r.Context(), id)
	if err != nil {
		s.log.Error("load account failed", "err", err)
		problem.Write(w, http.StatusInternalServerError, problem.TypeInternal, "")
		return
	}
	httpjson.Write(w, http.StatusOK, struct {
		AccountID string    `json:"account_id"`
		CreatedAt time.Time `json:"created_at"`
		// UTC always: pgx returns timestamptz in the process's local zone, so without this the
		// same instant serializes differently from a container (UTC) and a developer's machine.
	}{acct.ID, acct.CreatedAt.UTC()})
}

// RequireAuth admits requests carrying a valid access token and puts its account ID in the
// context. An expired token gets its own problem type, so a client knows a refresh will fix it;
// anything else wrong with the token is plain unauthorized.
func (s *Service) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
			problem.Write(w, http.StatusUnauthorized, problem.TypeUnauthorized, "send Authorization: Bearer <access token>")
			return
		}

		accountID, err := s.VerifyAccessToken(token)
		switch {
		case errors.Is(err, ErrAccessTokenExpired):
			problem.Write(w, http.StatusUnauthorized, problem.TypeTokenExpired, "refresh the session")
			return
		case err != nil:
			// The reason goes to the log at debug, never to the client.
			s.log.Debug("access token rejected", "err", err)
			problem.Write(w, http.StatusUnauthorized, problem.TypeUnauthorized, "")
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), accountIDKey{}, accountID)))
	})
}
