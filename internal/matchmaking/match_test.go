package matchmaking

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

var (
	coop = Modes["coop"]
	t0   = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
)

func at(seconds float64) time.Time { return t0.Add(time.Duration(seconds * float64(time.Second))) }

func tk(id string, players int, rating float64, createdSec float64) Ticket {
	return Ticket{ID: id, Players: players, Rating: rating, Created: at(createdSec)}
}

func TestMatchTable(t *testing.T) {
	p := DefaultPolicy // window ±100 +10/s, cap 1000, fill wait 10 s
	cases := []struct {
		name string
		pool []Ticket
		now  float64
		want [][]string
	}{
		{"four solos form a full match at once",
			[]Ticket{tk("a", 1, 1500, 0), tk("b", 1, 1500, 0), tk("c", 1, 1500, 0), tk("d", 1, 1500, 0)},
			0, [][]string{{"a", "b", "c", "d"}}},
		{"two solos wait for more players...",
			[]Ticket{tk("a", 1, 1500, 0), tk("b", 1, 1500, 0)},
			9.9, nil},
		{"...and form a pair once the oldest has waited the fill time",
			[]Ticket{tk("a", 1, 1500, 0), tk("b", 1, 1500, 0)},
			10, [][]string{{"a", "b"}}},
		{"a lone player never matches alone",
			[]Ticket{tk("a", 1, 1500, 0)},
			100, nil},
		{"a party of 2 plus a party of 2 is a full match",
			[]Ticket{tk("p1", 2, 1500, 0), tk("p2", 2, 1500, 1)},
			1, [][]string{{"p1", "p2"}}},
		{"parties are never split: 3 + 2 cannot share a 4-player match",
			// Equal ages, so the deterministic tie-break (ticket ID) puts p2 first.
			[]Ticket{tk("p3", 3, 1500, 0), tk("p2", 2, 1500, 0)},
			10, [][]string{{"p2"}, {"p3"}}},
		{"oldest first: the newest of five waits",
			[]Ticket{tk("e", 1, 1500, 4), tk("a", 1, 1500, 0), tk("c", 1, 1500, 2), tk("b", 1, 1500, 1), tk("d", 1, 1500, 3)},
			4, [][]string{{"a", "b", "c", "d"}}},
		{"every pair in a group must be compatible, not just each with the anchor",
			// a-b 90 and b-c 90 fit a ±100 window; a-c 180 does not, so c cannot join a and b.
			[]Ticket{tk("a", 1, 1500, 0), tk("b", 1, 1590, 0), tk("c", 1, 1680, 0), tk("d", 1, 1500, 0), tk("e", 1, 1500, 0)},
			0, [][]string{{"a", "b", "d", "e"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Match(coop, p, c.pool, at(c.now)); !equalGroups(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

// The window widens with waiting, and the narrower of two windows decides. Two players 300 apart,
// queued 10 s apart: the newer one's window (100 + 10 × its wait) reaches 300 only when it has
// waited 20 s, i.e. at t = 30. Before that, no match, however long the older one has waited.
func TestWindowWidensWithWait(t *testing.T) {
	p := DefaultPolicy
	pool := []Ticket{tk("old", 1, 1500, 0), tk("new", 1, 1800, 10)}

	if got := Match(coop, p, pool, at(29.9)); got != nil {
		t.Fatalf("matched at t=29.9 (newer window %.0f < 300): %v", p.Window(pool[1], at(29.9)), got)
	}
	if got := Match(coop, p, pool, at(30)); !equalGroups(got, [][]string{{"old", "new"}}) {
		t.Fatalf("not matched at t=30 (both windows >= 300): %v", got)
	}
}

func TestWindowIsCapped(t *testing.T) {
	p := DefaultPolicy
	pool := []Ticket{tk("a", 1, 1000, 0), tk("b", 1, 2100, 0)} // 1100 apart, cap 1000
	if got := Match(coop, p, pool, at(3600)); got != nil {
		t.Fatalf("matched beyond the window cap: %v", got)
	}
	if w := p.Window(pool[0], at(3600)); w != p.WindowCap {
		t.Fatalf("window after an hour = %v, want the cap %v", w, p.WindowCap)
	}
}

func TestExpired(t *testing.T) {
	p := DefaultPolicy
	pool := []Ticket{tk("old", 1, 1500, 0), tk("new", 1, 1500, 60)}
	if got := Expired(p, pool, at(119.9)); got != nil {
		t.Fatalf("expired before MaxWait: %v", got)
	}
	if got := Expired(p, pool, at(120)); !slices.Equal(got, []string{"old"}) {
		t.Fatalf("got %v", got)
	}
}

func TestMatchDoesNotReorderInput(t *testing.T) {
	pool := []Ticket{tk("b", 1, 1500, 1), tk("a", 1, 1500, 0)}
	Match(coop, DefaultPolicy, pool, at(10))
	if pool[0].ID != "b" {
		t.Fatal("Match reordered the caller's slice: a pure function must not")
	}
}

// Invariants over random pools, the property-test way: whatever the input, no ticket is in two
// matches, every match is a legal size, and every pair in it is compatible.
func TestMatchInvariantsRandom(t *testing.T) {
	p := DefaultPolicy
	rng := rand.New(rand.NewPCG(1, 2))
	for round := 0; round < 200; round++ {
		var pool []Ticket
		byID := map[string]Ticket{}
		for i := 0; i < 1+rng.IntN(60); i++ {
			tick := tk(fmt.Sprint("t", i), 1+rng.IntN(4), 1000+rng.Float64()*1000, rng.Float64()*60)
			pool = append(pool, tick)
			byID[tick.ID] = tick
		}
		now := at(rng.Float64() * 90)

		seen := map[string]bool{}
		for _, g := range Match(coop, p, pool, now) {
			size := 0
			for _, id := range g {
				if seen[id] {
					t.Fatalf("round %d: ticket %s in two matches", round, id)
				}
				seen[id] = true
				size += byID[id].Players
			}
			if size < coop.MinPlayers || size > coop.MaxPlayers {
				t.Fatalf("round %d: match of %d players", round, size)
			}
			for _, a := range g {
				for _, b := range g {
					if a != b && !p.compatible(byID[a], byID[b], now) {
						t.Fatalf("round %d: %s and %s are not compatible", round, a, b)
					}
				}
			}
		}
	}
}

// What one director round costs in CPU, before any Redis: the greedy pass is O(n²) in the worst
// case (every candidate checked against every anchor).
func BenchmarkMatch(b *testing.B) {
	for _, n := range []int{1000, 10000} {
		rng := rand.New(rand.NewPCG(3, 4))
		pool := make([]Ticket, n)
		for i := range pool {
			pool[i] = tk(fmt.Sprint("t", i), 1+rng.IntN(2), 1000+rng.Float64()*1000, rng.Float64()*60)
		}
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			for b.Loop() {
				Match(coop, DefaultPolicy, pool, at(60))
			}
		})
	}
}

func equalGroups(a, b [][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !slices.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}
