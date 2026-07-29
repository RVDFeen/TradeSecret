// Package risk turns account/portfolio state into position sizing and
// trading-halt decisions. It never talks to the broker directly.
package risk

import "math"

type Manager struct {
	RiskPerTradePct   float64
	MaxPositionPct    float64
	MaxPositions      int
	DailyLossLimitPct float64
}

// PositionSize returns the number of whole shares to buy given the account
// equity, entry price, and stop-loss price, so that a stop-out loses no more
// than RiskPerTradePct of equity. The real driver is that risk calculation —
// a tighter stop (the market saying this entry is lower-risk right now) sizes
// bigger, a wider stop sizes smaller, entirely on its own. MaxPositionPct and
// buying power are not sizing targets, just backstops: MaxPositionPct catches
// a degenerate case (e.g. a near-zero ATR blowing the risk math up to an
// absurd share count), and buying power is the hard real-world ceiling on
// what you can actually afford. Neither should be the thing that binds in
// normal operation — if MaxPositionPct is routinely capping every trade, it's
// set too low for the stop distances this strategy actually uses.
func (m Manager) PositionSize(equity, buyingPower, entryPrice, stopPrice float64) int64 {
	if entryPrice <= 0 || stopPrice >= entryPrice {
		return 0
	}
	riskDollars := equity * (m.RiskPerTradePct / 100.0)
	perShareRisk := entryPrice - stopPrice
	qtyByRisk := riskDollars / perShareRisk

	maxNotional := equity * (m.MaxPositionPct / 100.0)
	qtyByExposure := maxNotional / entryPrice

	qtyByBuyingPower := buyingPower / entryPrice

	qty := math.Min(qtyByRisk, math.Min(qtyByExposure, qtyByBuyingPower))
	return int64(math.Floor(qty))
}

// DailyLossBreached reports whether today's drawdown from the day's starting
// equity has exceeded the configured daily loss limit, meaning the bot
// should stop opening new positions (and the caller may choose to flatten).
func (m Manager) DailyLossBreached(dayStartEquity, currentEquity float64) bool {
	if dayStartEquity <= 0 {
		return false
	}
	drawdownPct := (dayStartEquity - currentEquity) / dayStartEquity * 100.0
	return drawdownPct >= m.DailyLossLimitPct
}

// CanOpenNewPosition reports whether another position can be opened given how
// many are already open.
func (m Manager) CanOpenNewPosition(openPositions int) bool {
	return openPositions < m.MaxPositions
}
