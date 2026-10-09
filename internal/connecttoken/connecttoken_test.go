package connecttoken

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

func keys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

var now = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

const (
	server = "127.0.0.1:7001"
	match  = "11111111-1111-4111-8111-111111111111"
	player = "22222222-2222-4222-8222-222222222222"
)

func TestRoundTrip(t *testing.T) {
	pub, priv := keys(t)
	tok, err := Mint(priv, match, player, server, now)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Verify(pub, tok, Expect{ServerAddr: server, MatchID: match}, now.Add(10*time.Second))
	if err != nil || c.AccountID != player || c.Nonce == "" {
		t.Fatalf("verify: %+v %v", c, err)
	}
}

// The plan's verification rows, one by one.
func TestRejections(t *testing.T) {
	pub, priv := keys(t)
	otherPub, _ := keys(t)
	tok, _ := Mint(priv, match, player, server, now)
	want := Expect{ServerAddr: server, MatchID: match}

	// Forged: change one byte of the payload. The signature no longer matches.
	head, sig, _ := strings.Cut(tok, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(head)
	forgedPayload := strings.Replace(string(payload), player, "33333333-3333-4333-8333-333333333333", 1)
	forged := base64.RawURLEncoding.EncodeToString([]byte(forgedPayload)) + "." + sig

	cases := []struct {
		name  string
		pub   ed25519.PublicKey
		token string
		want  Expect
		at    time.Time
		err   error
	}{
		{"forged payload", pub, forged, want, now, ErrSignature},
		{"signed by another key", otherPub, tok, want, now, ErrSignature},
		{"expired, past exp plus leeway", pub, tok, want, now.Add(TTL + Leeway + time.Second), ErrExpired},
		{"issued in the future, beyond leeway", pub, tok, want, now.Add(-Leeway - time.Second), ErrNotYetValid},
		{"for another server", pub, tok, Expect{ServerAddr: "127.0.0.1:7002", MatchID: match}, now, ErrWrongServer},
		{"for another match", pub, tok, Expect{ServerAddr: server, MatchID: "other"}, now, ErrWrongMatch},
		{"garbage", pub, "not-a-token", want, now, ErrMalformed},
		{"empty signature", pub, head + ".", want, now, ErrMalformed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Verify(c.pub, c.token, c.want, c.at); !errors.Is(err, c.err) {
				t.Fatalf("got %v, want %v", err, c.err)
			}
		})
	}

	// Within the leeway both ways is accepted: clocks between machines are never exact.
	if _, err := Verify(pub, tok, want, now.Add(TTL+Leeway-time.Second)); err != nil {
		t.Errorf("just inside the expiry leeway: %v", err)
	}
	if _, err := Verify(pub, tok, want, now.Add(-Leeway+time.Second)); err != nil {
		t.Errorf("just inside the issue leeway: %v", err)
	}
}

func TestKeyEncodings(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	rand.Read(seed)
	for name, enc := range map[string]string{
		"std padded": base64.StdEncoding.EncodeToString(seed),
		"url raw":    base64.RawURLEncoding.EncodeToString(seed),
	} {
		priv, err := ParsePrivateKey(enc)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		pub, err := ParsePublicKey(EncodePublicKey(priv.Public().(ed25519.PublicKey)))
		if err != nil || !pub.Equal(priv.Public()) {
			t.Fatalf("%s: public key round trip: %v", name, err)
		}
	}
	if _, err := ParsePrivateKey("c2hvcnQ="); err == nil {
		t.Fatal("a short seed was accepted")
	}
}
