package matchmaking

import (
	"math"
	"sort"
	"time"
)

// Mode is a kind of match the backend offers: how many players it takes. Modes are declared in
// code, like leaderboards are declared by migration: a client asking for a mode that does not
// exist gets a 400, not a new mode.
type Mode struct {
	Name       string
	MinPlayers int
	MaxPlayers int
}

// Modes the backend offers. The Proving Ground is co-op for 2 to 4.
var Modes = map[string]Mode{
	"coop": {Name: "coop", MinPlayers: 2, MaxPlayers: 4},
}

// Policy is how patient matching is. The values are product decisions, so they are named and in
// one place, not scattered as constants.
type Policy struct {
	// A ticket accepts partners whose rating is within ±Window of its own, where the window is
	// WindowBase + WindowRate × seconds waited, capped at WindowCap. Waiting widens tolerance:
	// a strong player alone at 3 a.m. should still get a game, just a less even one.
	WindowBase float64
	WindowRate float64
	WindowCap  float64

	// FillWait is how long a group short of MaxPlayers waits for more before forming anyway. A
	// full group forms at once. Without this, two players queueing a second apart would be
	// matched as a pair, and a 2-player party would match alone the instant it queued.
	FillWait time.Duration

	// MaxWait fails a ticket that found no match. The client may queue again.
	MaxWait time.Duration
}

var DefaultPolicy = Policy{
	WindowBase: 100,
	WindowRate: 10,
	WindowCap:  1000,
	FillWait:   10 * time.Second,
	MaxWait:    2 * time.Minute,
}

// Ticket is what the match function sees of a queued ticket.
type Ticket struct {
	ID      string
	Players int     // a party queues as one ticket of its size, and is never split
	Rating  float64 // the players' average
	Created time.Time
}

// Window is the rating tolerance of t at now.
func (p Policy) Window(t Ticket, now time.Time) float64 {
	waited := now.Sub(t.Created).Seconds()
	if waited < 0 {
		waited = 0
	}
	return math.Min(p.WindowBase+p.WindowRate*waited, p.WindowCap)
}

// compatible is the plan's rule: each ticket is inside the other's window. With different waits
// the windows differ, and the narrower one decides. The newcomer is not forced on the veteran,
// nor the other way round.
func (p Policy) compatible(a, b Ticket, now time.Time) bool {
	return math.Abs(a.Rating-b.Rating) <= math.Min(p.Window(a, now), p.Window(b, now))
}

// Match proposes matches from a mode's pool of queued tickets. It is a pure function: the same
// tickets and the same now always give the same groups. That is OpenMatch's split between the
// match function and the director, and the reason is testing. Every interesting rule (windows,
// parties, fill, fairness) is exercised as a table of inputs and outputs, with no Redis, no
// goroutines and no clock.
//
// The algorithm is greedy, oldest first: take the oldest unmatched ticket as an anchor, add the
// oldest tickets that fit (size) and are compatible with everyone already in the group, and stop
// at MaxPlayers. The group becomes a match if it is full, or if it has at least MinPlayers and
// its oldest ticket has waited FillWait. Otherwise its tickets stay in the pool for the next
// round, and the next anchor is tried.
//
// Greedy is not optimal. Maximising the number of matched players is an assignment problem, and
// greedy can miss a grouping a solver would find (sizes 3, 2, 2 with a max of 4: the 3 blocks the
// 2s). Shipped matchmakers are mostly greedy-by-age anyway, because wait time is what players
// feel. Recorded so nobody "fixes" this into a solver without a measured reason.
func Match(mode Mode, p Policy, pool []Ticket, now time.Time) [][]string {
	sorted := make([]Ticket, len(pool))
	copy(sorted, pool) // never reorder the caller's slice: pure means no side effects
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].Created.Equal(sorted[j].Created) {
			return sorted[i].Created.Before(sorted[j].Created)
		}
		return sorted[i].ID < sorted[j].ID // deterministic for equal times
	})

	used := make([]bool, len(sorted))
	var matches [][]string
	for i, anchor := range sorted {
		if used[i] || anchor.Players > mode.MaxPlayers {
			continue
		}
		group := []int{i}
		size := anchor.Players
		for j, cand := range sorted {
			if size == mode.MaxPlayers {
				break
			}
			if j == i || used[j] || size+cand.Players > mode.MaxPlayers {
				continue
			}
			fits := true
			for _, g := range group {
				if !p.compatible(sorted[g], cand, now) {
					fits = false
					break
				}
			}
			if fits {
				group = append(group, j)
				size += cand.Players
			}
		}

		// The group's oldest ticket decides the fill wait. sorted is oldest first, so that is the
		// lowest index in the group.
		oldest := group[0]
		for _, g := range group {
			if g < oldest {
				oldest = g
			}
		}
		full := size == mode.MaxPlayers
		filledEnough := size >= mode.MinPlayers && now.Sub(sorted[oldest].Created) >= p.FillWait
		if !full && !filledEnough {
			continue
		}

		ids := make([]string, len(group))
		for k, g := range group {
			used[g] = true
			ids[k] = sorted[g].ID
		}
		matches = append(matches, ids)
	}
	return matches
}

// Expired returns the tickets that have waited MaxWait. The director fails them after the round's
// matching, so a ticket at the very edge still gets its last chance to match first.
func Expired(p Policy, pool []Ticket, now time.Time) []string {
	var out []string
	for _, t := range pool {
		if now.Sub(t.Created) >= p.MaxWait {
			out = append(out, t.ID)
		}
	}
	return out
}
