// Package id generates random identifiers for things that never touch Postgres, where
// gen_random_uuid() would do the job: WebSocket connections, push messages, parties.
package id

import (
	"crypto/rand"
	"fmt"
)

// New returns a random version 4 UUID (RFC 9562): 122 random bits, so collisions are not a
// practical concern and the value is unguessable.
func New() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error on supported platforms
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
