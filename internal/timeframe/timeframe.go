// Package timeframe describes the bar resolution a strategy runs on, and the
// bits of math (cache keys, annualization) that depend on it.
package timeframe

import "fmt"

type Unit string

const (
	Minute Unit = "Min"
	Hour   Unit = "Hour"
	Day    Unit = "Day"
)

type Timeframe struct {
	N    int
	Unit Unit
}

var (
	OneMinute = Timeframe{N: 1, Unit: Minute}
	OneHour   = Timeframe{N: 1, Unit: Hour}
	OneDay    = Timeframe{N: 1, Unit: Day}
)

// String is used as an on-disk cache key, e.g. "1Hour", "1Day".
func (t Timeframe) String() string {
	return fmt.Sprintf("%d%s", t.N, t.Unit)
}

// PeriodsPerYear estimates how many bars of this size occur in a trading
// year, assuming a ~6.5 hour US equities session and 252 trading days/year.
// Used to annualize backtest return statistics (Sharpe).
func (t Timeframe) PeriodsPerYear() float64 {
	const tradingDaysPerYear = 252.0
	const hoursPerSession = 6.5
	switch t.Unit {
	case Hour:
		return tradingDaysPerYear * hoursPerSession / float64(t.N)
	case Minute:
		return tradingDaysPerYear * hoursPerSession * 60 / float64(t.N)
	default:
		return tradingDaysPerYear / float64(t.N)
	}
}
