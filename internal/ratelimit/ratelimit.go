// Package ratelimit turns Alpaca's documented API rate limit into concrete
// numbers the engine can plan against: how fast is it safe to poll at all,
// and — the more interesting question — how many candidate symbols can
// safely be scanned for new entries in a single tick.
//
// The core idea: protecting existing positions (position-count-independent,
// one batched call to check, only paying per-position when one is actually
// found unprotected) and checking account/fill state are safety-critical and
// must happen in full every tick. Scanning candidates for new entries is the
// part that scales with universe size, so it's the part that gets throttled
// when budget is tight — never the position-protection side. Crucially, the
// budget for candidate-scanning is computed from what protection *actually*
// cost this tick, not a worst-case assumption that every held position needs
// re-protecting — that assumption alone can eat the entire budget once
// MAX_POSITIONS is more than a handful, since it scales with position count
// even though real protection cost almost never does (unprotected positions
// are the rare exception, not the norm).
package ratelimit

import (
	"math"
	"time"
)

// AlpacaCallsPerMinute is Alpaca's documented free-tier market-data/trading
// API rate limit.
const AlpacaCallsPerMinute = 200.0

// SafetyFraction is how much of the budget this bot plans against, leaving
// headroom for bursts within a tick (calls fire in a few seconds, not spread
// evenly across the poll interval) and anything else sharing the account
// (e.g. running `backtest` by hand while the bot is live).
const SafetyFraction = 0.75

// SafeBudgetPerMinute is the actual ceiling planned against.
const SafeBudgetPerMinute = AlpacaCallsPerMinute * SafetyFraction

// AlwaysEveryTickCalls: GetOpenPositions, HasLiveProtectiveStop (both single
// batched calls regardless of how many positions exist), GetFillActivities,
// GetAccount, OpenPositionSymbols, OpenOrderSymbols. Fixed, doesn't scale
// with MAX_POSITIONS.
const AlwaysEveryTickCalls = 6.0

// PerUnprotectedPositionCalls: actual cost per position genuinely found
// missing its protective stop this tick — bars fetch + latest price + place
// protective order. Charged only for positions actually fixed, not reserved
// upfront for every position that could theoretically need it.
const PerUnprotectedPositionCalls = 3.0

// PerEntryCalls: latest price + place order, for one successful new entry.
const PerEntryCalls = 2.0

// MinPollInterval is the fastest poll interval that can safely cover the
// fixed always-every-tick cost alone (position/order/account/fill checks),
// independent of MAX_POSITIONS since none of those calls scale with position
// count. This is the hard floor: nothing should ever poll faster than this.
func MinPollInterval() time.Duration {
	minutes := AlwaysEveryTickCalls / SafeBudgetPerMinute
	seconds := math.Ceil(minutes * 60)
	return time.Duration(seconds) * time.Second
}

// MaxCandidatesPerTick is how many new-entry candidates (each costing one
// bars-fetch call) can safely be scanned this tick, given:
//   - pollInterval: the tick cadence, which sets the total budget available.
//   - callsSpentSoFar: what protection/account/fill checks *actually* cost
//     this tick (AlwaysEveryTickCalls + PerUnprotectedPositionCalls for each
//     position genuinely fixed) — real, not a worst-case guess.
//   - availableSlots: how many more positions could still be opened
//     (MAX_POSITIONS minus what's currently held), reserving PerEntryCalls
//     for each in case they all fill this tick.
//
// Always at least 1, so candidate scanning never fully stops even under a
// very tight budget — it just slows down.
func MaxCandidatesPerTick(pollInterval time.Duration, callsSpentSoFar float64, availableSlots int) int {
	budget := SafeBudgetPerMinute * pollInterval.Minutes()
	remaining := budget - callsSpentSoFar - PerEntryCalls*float64(availableSlots)
	n := int(remaining)
	if n < 1 {
		n = 1
	}
	return n
}
