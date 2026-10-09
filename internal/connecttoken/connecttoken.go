// Package connecttoken mints and verifies the tokens a player presents to a game server.
//
// The backend signs with an Ed25519 private key; game servers verify with the public key only.
// That asymmetry is the point. A game server is the least trusted process in the system (it runs
// wherever there is capacity, and it is what cheaters attack), and holding only the public key, it
// can check tokens but never mint one. HMAC would put the minting secret on every server.
//
// Format (docs/api/connect-token.md):
//
//	base64url(payload JSON) "." base64url(Ed25519 signature over the first part's bytes)
//
// The signature covers the encoded payload, not a re-serialization of it, so a verifier in any
// language checks exactly the bytes it received. JSON canonicalization never comes into it.
package connecttoken

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Version is the payload format. A verifier refuses any other value, so changing the format means
// a deliberate new version, never a silent reinterpretation.
const Version = 1

// TTL is how long a token can be used to connect. Short on purpose: it admits a player to one
// match on one server, once. It is not a session.
const TTL = 30 * time.Second

// Leeway absorbs clock skew between the backend that minted a token and the server that checks
// it. B1 noted this would be needed: in B5 issuer and verifier are different machines.
const Leeway = 5 * time.Second

// Claims is the payload.
type Claims struct {
	V          int    `json:"v"`
	MatchID    string `json:"match_id"`
	AccountID  string `json:"account_id"`
	ServerAddr string `json:"server_addr"`
	IssuedAt   int64  `json:"iat"` // unix seconds
	ExpiresAt  int64  `json:"exp"` // unix seconds
	Nonce      string `json:"nonce"`
}

var (
	ErrMalformed   = errors.New("malformed connect token")
	ErrSignature   = errors.New("bad connect token signature")
	ErrVersion     = errors.New("unsupported connect token version")
	ErrExpired     = errors.New("connect token expired")
	ErrNotYetValid = errors.New("connect token issued in the future")
	ErrWrongServer = errors.New("connect token is for another server")
	ErrWrongMatch  = errors.New("connect token is for another match")
)

// Mint signs a token for one player, one match, one server.
func Mint(key ed25519.PrivateKey, matchID, accountID, serverAddr string, now time.Time) (string, error) {
	var n [16]byte
	if _, err := rand.Read(n[:]); err != nil {
		return "", err
	}
	c := Claims{
		V: Version, MatchID: matchID, AccountID: accountID, ServerAddr: serverAddr,
		IssuedAt: now.Unix(), ExpiresAt: now.Add(TTL).Unix(),
		Nonce: base64.RawURLEncoding.EncodeToString(n[:]),
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode claims: %w", err)
	}
	head := base64.RawURLEncoding.EncodeToString(payload)
	sig := ed25519.Sign(key, []byte(head))
	return head + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Expect is what a server knows about itself when it checks a token.
type Expect struct {
	ServerAddr string // this server's advertised address
	MatchID    string // the match it was allocated
}

// Verify checks a token's signature first, and only then reads its claims: nothing in an
// unauthenticated payload is trusted, not even its version. Replay (the same token twice) is the
// caller's job, because it needs memory across calls; the nonce is what to remember.
func Verify(pub ed25519.PublicKey, token string, want Expect, now time.Time) (Claims, error) {
	head, sigPart, ok := strings.Cut(token, ".")
	if !ok || head == "" || sigPart == "" {
		return Claims{}, ErrMalformed
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigPart)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return Claims{}, ErrMalformed
	}
	if !ed25519.Verify(pub, []byte(head), sig) {
		return Claims{}, ErrSignature
	}

	payload, err := base64.RawURLEncoding.DecodeString(head)
	if err != nil {
		return Claims{}, ErrMalformed
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return Claims{}, ErrMalformed
	}
	switch {
	case c.V != Version:
		return Claims{}, ErrVersion
	case now.After(time.Unix(c.ExpiresAt, 0).Add(Leeway)):
		return Claims{}, ErrExpired
	case now.Before(time.Unix(c.IssuedAt, 0).Add(-Leeway)):
		return Claims{}, ErrNotYetValid
	case c.ServerAddr != want.ServerAddr:
		return Claims{}, ErrWrongServer
	case c.MatchID != want.MatchID:
		return Claims{}, ErrWrongMatch
	case c.AccountID == "" || c.Nonce == "":
		return Claims{}, ErrMalformed
	}
	return c, nil
}

// ParsePrivateKey decodes the backend's signing key: a base64 (standard or URL, padded or not)
// Ed25519 seed of 32 bytes. A seed, not the 64-byte private key form, because a seed is what
// `openssl rand -base64 32` produces.
func ParsePrivateKey(s string) (ed25519.PrivateKey, error) {
	seed, err := decodeAnyBase64(s)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("want a base64-encoded %d-byte Ed25519 seed", ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// EncodePublicKey and ParsePublicKey are the public key's wire form: base64url, no padding.
func EncodePublicKey(pub ed25519.PublicKey) string { return base64.RawURLEncoding.EncodeToString(pub) }

func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := decodeAnyBase64(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("want a base64-encoded %d-byte Ed25519 public key", ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}

func decodeAnyBase64(s string) ([]byte, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "=")
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}
