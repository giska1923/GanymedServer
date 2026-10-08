package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/giska1923/GanymedServer/internal/problem"
)

type requestIDKey struct{}

// RequestID returns the ID the middleware assigned to this request, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// Middleware wraps the mux in the order that matters, outermost first:
// request ID (so everything after can log it) → logging (so it sees the final status, including a
// recovered panic's 500) → recover.
func Middleware(next http.Handler, log *slog.Logger) http.Handler {
	return withRequestID(withLogging(withRecover(next, log), log))
}

// withRequestID always generates the ID. An incoming X-Request-Id is not trusted: a client could
// otherwise inject arbitrary text into every log line of the request.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b [8]byte
		_, _ = rand.Read(b[:])
		id := hex.EncodeToString(b[:])
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the real writer through this wrapper, which B3's
// deadline changes and hijacking depend on.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func withLogging(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// r.Pattern, not r.URL.Path: the matched route ("GET /v1/leaderboards/{board}") groups
		// requests usefully and never logs an ID or anything a client typed into the path.
		// It is empty for a request that matched no route.
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		log.LogAttrs(r.Context(), slog.LevelInfo, "request",
			slog.String("request_id", RequestID(r.Context())),
			slog.String("route", route),
			slog.Int("status", rec.status),
			slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
		)
	})
}

func withRecover(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			// http.ErrAbortHandler is net/http's own way to abort a response; re-panic so the
			// server handles it as designed rather than logging it as a bug.
			if v == http.ErrAbortHandler {
				panic(v)
			}
			log.Error("panic in handler",
				"request_id", RequestID(r.Context()), "panic", v, "stack", string(debug.Stack()))
			problem.Write(w, http.StatusInternalServerError, problem.TypeInternal, "")
		}()
		next.ServeHTTP(w, r)
	})
}
