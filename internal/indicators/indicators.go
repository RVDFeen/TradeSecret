// Package indicators computes technical indicators over price series.
package indicators

// SMA returns the simple moving average of the last `period` values in v.
// Returns (0, false) if there isn't enough data.
func SMA(v []float64, period int) (float64, bool) {
	if len(v) < period || period <= 0 {
		return 0, false
	}
	sum := 0.0
	for _, x := range v[len(v)-period:] {
		sum += x
	}
	return sum / float64(period), true
}

// EMASeries returns the exponential moving average series for the given period,
// using SMA of the first `period` values as the seed. The returned slice is
// shorter than v by (period-1) elements; EMASeries(v,period)[i] corresponds to
// v[period-1+i].
func EMASeries(v []float64, period int) []float64 {
	if len(v) < period || period <= 0 {
		return nil
	}
	k := 2.0 / float64(period+1)
	seed, _ := SMA(v[:period], period)
	out := make([]float64, len(v)-period+1)
	out[0] = seed
	prev := seed
	for i := period; i < len(v); i++ {
		e := v[i]*k + prev*(1-k)
		out[i-period+1] = e
		prev = e
	}
	return out
}

// EMA returns just the latest EMA value for the given period.
func EMA(v []float64, period int) (float64, bool) {
	s := EMASeries(v, period)
	if len(s) == 0 {
		return 0, false
	}
	return s[len(s)-1], true
}

// RSI returns the latest Wilder's RSI(period) value for the closes series.
func RSI(closes []float64, period int) (float64, bool) {
	if len(closes) < period+1 {
		return 0, false
	}
	gains, losses := 0.0, 0.0
	for i := 1; i <= period; i++ {
		diff := closes[i] - closes[i-1]
		if diff > 0 {
			gains += diff
		} else {
			losses += -diff
		}
	}
	avgGain := gains / float64(period)
	avgLoss := losses / float64(period)

	for i := period + 1; i < len(closes); i++ {
		diff := closes[i] - closes[i-1]
		gain, loss := 0.0, 0.0
		if diff > 0 {
			gain = diff
		} else {
			loss = -diff
		}
		avgGain = (avgGain*float64(period-1) + gain) / float64(period)
		avgLoss = (avgLoss*float64(period-1) + loss) / float64(period)
	}

	if avgLoss == 0 {
		return 100, true
	}
	rs := avgGain / avgLoss
	rsi := 100 - (100 / (1 + rs))
	return rsi, true
}

// ATR returns the latest Wilder's Average True Range(period) given highs, lows, closes.
// All three slices must be the same length and time-aligned.
func ATR(highs, lows, closes []float64, period int) (float64, bool) {
	n := len(closes)
	if n < period+1 || len(highs) != n || len(lows) != n {
		return 0, false
	}
	trueRanges := make([]float64, n-1)
	for i := 1; i < n; i++ {
		hl := highs[i] - lows[i]
		hc := abs(highs[i] - closes[i-1])
		lc := abs(lows[i] - closes[i-1])
		tr := hl
		if hc > tr {
			tr = hc
		}
		if lc > tr {
			tr = lc
		}
		trueRanges[i-1] = tr
	}

	atr := 0.0
	for _, tr := range trueRanges[:period] {
		atr += tr
	}
	atr /= float64(period)

	for i := period; i < len(trueRanges); i++ {
		atr = (atr*float64(period-1) + trueRanges[i]) / float64(period)
	}
	return atr, true
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
