package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const issuer = "ganymedserver"

var (
	ErrAccessTokenExpired = errors.New("access token expired")
	ErrAccessTokenInvalid = errors.New("access token invalid")
)

// accessTokens issues and verifies the short-lived JWTs.
//
// HS256: one shared secret, because the issuer and the verifier are the same binary. Asymmetric
// signing earns its keep when something else verifies without being able to mint, which is B5's
// connect tokens, not this.
type accessTokens struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

func (a accessTokens) issue(accountID string) (string, error) {
	now := a.now()
	claims := jwt.RegisteredClaims{
		Issuer:    issuer,
		Subject:   accountID,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(a.ttl)),
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(a.secret)
	if err != nil {
		return "", fmt.Errorf("sign access token: %w", err)
	}
	return s, nil
}

// verify returns the account ID in a valid token.
//
// WithValidMethods is the line that matters. Without it, the token's own "alg" header chooses the
// verification algorithm, which is the classic JWT vulnerability: "alg": "none" skips the
// signature entirely, and algorithm confusion tricks an RSA verifier into treating its public key
// as an HMAC secret. The server decides the algorithm; the token does not.
func (a accessTokens) verify(token string) (string, error) {
	claims := &jwt.RegisteredClaims{}
	_, err := jwt.ParseWithClaims(token, claims,
		func(*jwt.Token) (any, error) { return a.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(issuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(a.now),
	)
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return "", ErrAccessTokenExpired
	case err != nil:
		return "", fmt.Errorf("%w: %v", ErrAccessTokenInvalid, err)
	case claims.Subject == "":
		return "", fmt.Errorf("%w: no subject", ErrAccessTokenInvalid)
	}
	return claims.Subject, nil
}

// newRefreshToken returns 256 random bits, base64url-encoded. Opaque on purpose: it means nothing
// except "look me up", which is what makes it revocable.
func newRefreshToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate refresh token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// hashSecret is how every high-entropy secret (device IDs, refresh tokens) is stored. See the note
// on the devices table in migrations/0001_auth.sql for why SHA-256 and not bcrypt.
func hashSecret(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
