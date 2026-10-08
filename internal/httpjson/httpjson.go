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
	"reflect"
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
		var wrongType *json.UnmarshalTypeError
		switch {
		case errors.As(err, &tooBig):
			return fmt.Errorf("body exceeds %d bytes", MaxBody)
		case errors.As(err, &wrongType):
			// The raw message names Go types ("Go struct field .score of type int64"); a client
			// should see JSON terms instead.
			return fmt.Errorf("field %q: got %s, want %s", wrongType.Field, wrongType.Value, jsonKind(wrongType.Type))
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

// jsonKind names a Go type the way the API contract does.
func jsonKind(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "an integer"
	case reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "a boolean"
	case reflect.Slice, reflect.Array:
		return "an array"
	default:
		return "an object"
	}
}

// Write sends v as JSON with the given status.
func Write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // a JSON API, not HTML: keep "<" rather than the escape <
	_ = enc.Encode(v)
}
