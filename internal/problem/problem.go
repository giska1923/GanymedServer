// Package problem writes RFC 9457 problem details (application/problem+json).
//
// Every error response the backend sends goes through here, so clients get one shape. Type is
// the stable, machine-readable part: the engine switches on it. Title is for humans and may be
// reworded; Detail describes this occurrence.
package problem

import (
	"encoding/json"
	"net/http"
)

// Types are URNs: absolute URIs, as RFC 9457 asks, that nobody will try to dereference. Each one
// is part of the contract and is listed in docs/api/openapi.yaml.
const (
	TypeInvalidRequest      = "urn:ganymed:problem:invalid-request"
	TypeUnauthorized        = "urn:ganymed:problem:unauthorized"
	TypeTokenExpired        = "urn:ganymed:problem:token-expired"
	TypeInvalidRefreshToken = "urn:ganymed:problem:invalid-refresh-token"
	TypeNotFound            = "urn:ganymed:problem:not-found"
	TypeInternal            = "urn:ganymed:problem:internal"
)

type Details struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

var titles = map[string]string{
	TypeInvalidRequest:      "The request is malformed",
	TypeUnauthorized:        "Authentication is required",
	TypeTokenExpired:        "The access token has expired",
	TypeInvalidRefreshToken: "The refresh token is not valid",
	TypeNotFound:            "No such resource",
	TypeInternal:            "Internal error",
}

// Write sends a problem response. detail must never contain secrets or internal error text:
// for 5xx responses pass "" and log the real error server-side.
func Write(w http.ResponseWriter, status int, typ, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	if status == http.StatusUnauthorized {
		// RFC 6750: a 401 for a bearer-token-protected resource names the scheme.
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // a JSON API, not HTML: keep "<" rather than the escape <
	_ = enc.Encode(Details{Type: typ, Title: titles[typ], Status: status, Detail: detail})
}
