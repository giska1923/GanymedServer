// Package httpjson decodes JSON request bodies and writes JSON responses the same way in every
// module: bounded body size, one JSON value per body, and errors a handler can turn straight into
// a 400.
package httpjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// MaxBody bounds every request body. Without a limit, json.Decoder reads whatever it is sent, so
// one request could exhaust memory. No request in the API comes near this.
const MaxBody = 64 << 10

// Decode reads exactly one JSON value into v. Unknown fields are accepted and ignored, so a newer
// client that sends a field an older server does not know still works; the robustness principle,
// chosen over strictness because client and server are deployed separately.
func Decode(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBody))
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			return fmt.Errorf("body exceeds %d bytes", MaxBody)
		case errors.Is(err, io.EOF):
			return errors.New("body is empty")
		default:
			return fmt.Errorf("body is not valid JSON: %w", err)
		}
	}
	if dec.More() {
		return errors.New("body contains more than one JSON value")
	}
	return nil
}

// Write sends v as JSON with the given status.
func Write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // a JSON API, not HTML: keep "<" rather than the escape <
	_ = enc.Encode(v)
}
