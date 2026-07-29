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
// than RiskPerTradePct of equity, while never exceeding MaxPositionPct of
// equity in a single name or the available buying power.
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
