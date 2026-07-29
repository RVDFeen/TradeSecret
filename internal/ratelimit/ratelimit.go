// Package ratelimit turns Alpaca's documented API rate limit into concrete
// numbers the engine can plan against: how fast is it safe to poll at all,
// and — the more interesting question — how many candidate symbols can
// safely be scanned for new entries in a single tick.
//
// The core idea: protecting existing positions (position-count-independent,
// one batched call) and checking account/fill state are safety-critical and
// must happen in full every tick. Scanning candidates for new entries is the
// part that scales with universe size, so it's the part that gets throttled
// when budget is tight — never the position-protection side.
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

// alwaysEveryTickCalls: GetOpenPositions, HasLiveProtectiveStop,
// GetFillActivities, GetAccount, OpenPositionSymbols, OpenOrderSymbols.
const alwaysEveryTickCalls = 6.0

// perUnprotectedPositionCalls: worst case for one held position missing its
// protective stop — bars fetch + latest price + place protective order.
const perUnprotectedPositionCalls = 3.0

// perEntryCalls: latest price + place order, for one successful new entry.
const perEntryCalls = 2.0

// BaselineCallsPerTick is the cost that must happen every single tick
// regardless of universe size, sized for the worst case where every held
// position needs its protective stop re-attached.
func BaselineCallsPerTick(maxPositions int) float64 {
	return alwaysEveryTickCalls + perUnprotectedPositionCalls*float64(maxPositions)
}

// MinPollInterval is the fastest poll interval that can safely cover just
// the baseline (position-protection + account state) cost, ignoring
// candidate scanning entirely. This is the hard floor: nothing should ever
// poll faster than this, no matter the universe size or mode.
func MinPollInterval(maxPositions int) time.Duration {
	minutes := BaselineCallsPerTick(maxPositions) / SafeBudgetPerMinute
	seconds := math.Ceil(minutes * 60)
	return time.Duration(seconds) * time.Second
}

// MaxCandidatesPerTick is how many new-entry candidates (each costing one
// bars-fetch call) can safely be scanned in a single tick at the given poll
// interval, after reserving budget for the baseline and a worst-case round
// of successful entries. Always at least 1, so candidate scanning never
// fully stops even under a very tight budget — it just slows down.
func MaxCandidatesPerTick(pollInterval time.Duration, maxPositions int) int {
	budget := SafeBudgetPerMinute * pollInterval.Minutes()
	remaining := budget - BaselineCallsPerTick(maxPositions) - perEntryCalls*float64(maxPositions)
	n := int(remaining)
	if n < 1 {
		n = 1
	}
	return n
}
