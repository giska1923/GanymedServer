package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

func fixedTokens(at time.Time) accessTokens {
	return accessTokens{secret: testSecret, ttl: 15 * time.Minute, now: func() time.Time { return at }}
}

func TestAccessTokenRoundTrip(t *testing.T) {
	tok := fixedTokens(time.Now())
	s, err := tok.issue("acct-1")
	if err != nil {
		t.Fatal(err)
	}
	id, err := tok.verify(s)
	if err != nil || id != "acct-1" {
		t.Fatalf("verify = %q, %v", id, err)
	}
}

func TestAccessTokenExpiredIsDistinguished(t *testing.T) {
	issued := time.Now()
	s, _ := fixedTokens(issued).issue("acct-1")

	_, err := fixedTokens(issued.Add(16 * time.Minute)).verify(s)
	if !errors.Is(err, ErrAccessTokenExpired) {
		t.Fatalf("got %v, want ErrAccessTokenExpired", err)
	}
}

func TestAccessTokenRejections(t *testing.T) {
	now := time.Now()
	claims := jwt.RegisteredClaims{
		Issuer: issuer, Subject: "acct-1",
		IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
	}
	sign := func(m jwt.SigningMethod, key any, c jwt.Claims) string {
		s, err := jwt.NewWithClaims(m, c).SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	wrongIssuer := claims
	wrongIssuer.Issuer = "someone-else"
	noExpiry := claims
	noExpiry.ExpiresAt = nil

	cases := map[string]string{
		"alg none": sign(jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, claims),
		// Same secret, different HMAC: only the algorithm pin rejects this.
		"HS512 not HS256": sign(jwt.SigningMethodHS512, testSecret, claims),
		"wrong secret":    sign(jwt.SigningMethodHS256, []byte("another-secret-another-secret-00"), claims),
		"wrong issuer":    sign(jwt.SigningMethodHS256, testSecret, wrongIssuer),
		"no expiry":       sign(jwt.SigningMethodHS256, testSecret, noExpiry),
		"garbage":         "not.a.jwt",
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := fixedTokens(now).verify(token)
			if !errors.Is(err, ErrAccessTokenInvalid) {
				t.Fatalf("got %v, want ErrAccessTokenInvalid", err)
			}
		})
	}
}
