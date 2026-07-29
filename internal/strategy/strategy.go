// Package strategy implements a long-only trend + momentum signal with
// volatility-based stops, evaluated on daily bars.
//
// Entry: EMA(fast) > EMA(slow) and price > EMA(slow) [uptrend], with RSI in a
// "healthy momentum" band (not overbought, not falling-knife oversold).
// Exit (in addition to the broker-side bracket stop/take-profit): if the
// trend flips, i.e. EMA(fast) crosses back below EMA(slow).
package strategy

import (
	"tradebot/internal/bar"
	"tradebot/internal/indicators"
)

type Params struct {
	EMAFastPeriod int
	EMASlowPeriod int
	RSIPeriod     int
	RSILowerBound float64
	RSIUpperBound float64
	ATRPeriod     int
	StopATRMult   float64
	TakeATRMult   float64
	// DisableTrendExit, when true, means positions are only closed by the
	// broker-side stop-loss/take-profit bracket, never early on a trend flip.
	DisableTrendExit bool
}

// DefaultParams were chosen by walk-forward backtesting a few candidate
// configurations over 1.5-5 year historical windows (see `tradebot backtest`)
// and picking the one that stayed net-positive with a reasonable Sharpe ratio
// across all of them, rather than the single best result on any one window.
func DefaultParams() Params {
	return Params{
		EMAFastPeriod:    20,
		EMASlowPeriod:    50,
		RSIPeriod:        14,
		RSILowerBound:    40,
		RSIUpperBound:    70,
		ATRPeriod:        14,
		StopATRMult:      2.5,
		TakeATRMult:      4.0,
		DisableTrendExit: true,
	}
}

type Signal struct {
	Price       float64
	EMAFast     float64
	EMASlow     float64
	RSI         float64
	ATR         float64
	Uptrend     bool    // EMA fast > EMA slow and price above EMA slow
	Momentum    bool    // RSI within the configured band
	ShouldEnter bool    // Uptrend && Momentum
	StopPrice   float64 // suggested stop-loss if entering now
	TakePrice   float64 // suggested take-profit if entering now
}

// MinBars returns how many bars are needed before Evaluate can produce a result.
func (p Params) MinBars() int {
	m := p.EMASlowPeriod
	if p.RSIPeriod+1 > m {
		m = p.RSIPeriod + 1
	}
	if p.ATRPeriod+1 > m {
		m = p.ATRPeriod + 1
	}
	return m + 5 // small buffer so EMA/ATR have settled past their seed window
}

// Evaluate computes the current signal from a time-ordered slice of bars
// (oldest first, last element is the most recent completed bar).
func Evaluate(bars []bar.Bar, p Params) (Signal, bool) {
	if len(bars) < p.MinBars() {
		return Signal{}, false
	}
	closes := bar.Closes(bars)
	highs := bar.Highs(bars)
	lows := bar.Lows(bars)

	emaFast, ok1 := indicators.EMA(closes, p.EMAFastPeriod)
	emaSlow, ok2 := indicators.EMA(closes, p.EMASlowPeriod)
	rsi, ok3 := indicators.RSI(closes, p.RSIPeriod)
	atr, ok4 := indicators.ATR(highs, lows, closes, p.ATRPeriod)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return Signal{}, false
	}

	price := closes[len(closes)-1]
	uptrend := emaFast > emaSlow && price > emaSlow
	momentum := rsi >= p.RSILowerBound && rsi <= p.RSIUpperBound

	sig := Signal{
		Price:       price,
		EMAFast:     emaFast,
		EMASlow:     emaSlow,
		RSI:         rsi,
		ATR:         atr,
		Uptrend:     uptrend,
		Momentum:    momentum,
		ShouldEnter: uptrend && momentum,
		StopPrice:   price - p.StopATRMult*atr,
		TakePrice:   price + p.TakeATRMult*atr,
	}
	return sig, true
}
