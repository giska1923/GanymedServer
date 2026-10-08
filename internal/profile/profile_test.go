package profile

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/giska1923/GanymedServer/internal/db/dbtest"
)

func TestDefaultDisplayName(t *testing.T) {
	if got := DefaultDisplayName("7f3a9c12-0000-4000-8000-000000000000"); got != "Player-7F3A9C" {
		t.Fatalf("got %q", got)
	}
}

func TestDisplayNameValidation(t *testing.T) {
	s := NewService(dbtest.New(t), slog.New(slog.DiscardHandler))
	ctx := context.Background()
	const id = "11111111-1111-4111-8111-111111111111"

	for name, ok := range map[string]bool{
		"Ann":                       true,
		"  Padded Name  ":           true, // trimmed first
		"x_y-z.9":                   true,
		"ab":                        false, // too short
		"abcdefghijklmnopqrstuvwxy": false, // 25: too long
		"-dash":                     false, // must start alphanumeric
		"trailing.":                 false, // must end alphanumeric
		"Zoë":                       false, // ASCII only (the HUD font)
		"tab\there":                 false,
	} {
		_, err := s.SetDisplayName(ctx, id, name)
		if ok && err != nil {
			t.Errorf("%q rejected: %v", name, err)
		}
		if !ok && !errors.Is(err, ErrInvalidDisplayName) {
			t.Errorf("%q: got %v, want ErrInvalidDisplayName", name, err)
		}
	}
}

func TestDisplayNamesMixStoredAndDefault(t *testing.T) {
	s := NewService(dbtest.New(t), slog.New(slog.DiscardHandler))
	ctx := context.Background()
	renamed := "22222222-2222-4222-8222-222222222222"
	untouched := "33333333-3333-4333-8333-333333333333"

	if _, err := s.SetDisplayName(ctx, renamed, "First"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDisplayName(ctx, renamed, "Second"); err != nil { // the upsert's update path
		t.Fatal(err)
	}

	names, err := s.DisplayNames(ctx, []string{renamed, untouched})
	if err != nil {
		t.Fatal(err)
	}
	if names[renamed] != "Second" || names[untouched] != "Player-333333" || len(names) != 2 {
		t.Fatalf("got %v", names)
	}

	var rows int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM profiles").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("%d profile rows, want 1: an untouched account must not get a row", rows)
	}
}
