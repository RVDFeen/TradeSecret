// Package broker wraps the Alpaca trading and market-data REST clients into
// the small surface the trading engine and backtester actually need.
package broker

import (
	"fmt"
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/v3/alpaca"
	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
	"github.com/shopspring/decimal"

	"tradebot/internal/bar"
	"tradebot/internal/config"
	"tradebot/internal/timeframe"
)

type Broker struct {
	trading *alpaca.Client
	data    *marketdata.Client
}

func New(cfg *config.Config) *Broker {
	trading := alpaca.NewClient(alpaca.ClientOpts{
		APIKey:    cfg.APIKeyID,
		APISecret: cfg.APISecret,
		BaseURL:   cfg.BaseURL,
	})
	data := marketdata.NewClient(marketdata.ClientOpts{
		APIKey:    cfg.APIKeyID,
		APISecret: cfg.APISecret,
		Feed:      marketdata.IEX, // included in the free market data plan
	})
	return &Broker{trading: trading, data: data}
}

// majorExchanges excludes OTC and similar thin/unreliable venues.
var majorExchanges = map[string]bool{
	"NYSE": true, "NASDAQ": true, "ARCA": true, "BATS": true, "AMEX": true,
}

// isPlainTicker filters out warrants, preferred shares, test symbols, and
// other non-standard tickers that tend to be illiquid or behave oddly for a
// trend-following strategy — plain common-stock tickers only.
func isPlainTicker(s string) bool {
	if len(s) == 0 || len(s) > 5 {
		return false
	}
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// GetTradableSymbols returns every tradable, active US equity common-stock
// symbol on a major exchange — the broad universe the daily liquidity
// ranking picks its shortlist from.
func (b *Broker) GetTradableSymbols() ([]string, error) {
	assets, err := b.trading.GetAssets(alpaca.GetAssetsRequest{
		Status:     "active",
		AssetClass: "us_equity",
	})
	if err != nil {
		return nil, fmt.Errorf("get assets: %w", err)
	}
	out := make([]string, 0, len(assets))
	for _, a := range assets {
		if !a.Tradable || !majorExchanges[a.Exchange] || !isPlainTicker(a.Symbol) {
			continue
		}
		out = append(out, a.Symbol)
	}
	return out, nil
}

// multiBarsChunkSize bounds how many symbols go into a single GetMultiBars
// call, so one oversized request doesn't risk hitting a server-side limit.
const multiBarsChunkSize = 200

// GetMultiRecentDailyBars fetches the last lookbackDays of daily bars for
// many symbols at once (chunked), for the daily liquidity ranking. This is
// what makes ranking thousands of symbols feasible within rate limits: a
// batch call instead of one request per symbol.
func (b *Broker) GetMultiRecentDailyBars(symbols []string, lookbackDays int) (map[string][]bar.Bar, error) {
	end := time.Now()
	start := end.AddDate(0, 0, -lookbackDays)

	out := make(map[string][]bar.Bar, len(symbols))
	for i := 0; i < len(symbols); i += multiBarsChunkSize {
		j := i + multiBarsChunkSize
		if j > len(symbols) {
			j = len(symbols)
		}
		raw, err := b.data.GetMultiBars(symbols[i:j], marketdata.GetBarsRequest{
			TimeFrame:  marketdata.OneDay,
			Start:      start,
			End:        end,
			Feed:       marketdata.IEX,
			Adjustment: marketdata.Raw,
		})
		if err != nil {
			return nil, fmt.Errorf("get multi bars (symbols %d-%d): %w", i, j, err)
		}
		for sym, bars := range raw {
			converted := make([]bar.Bar, len(bars))
			for k, x := range bars {
				converted[k] = bar.Bar{
					Time: x.Timestamp, Open: x.Open, High: x.High, Low: x.Low, Close: x.Close, Volume: float64(x.Volume),
				}
			}
			out[sym] = converted
		}
	}
	return out, nil
}

func (b *Broker) GetClock() (*alpaca.Clock, error) {
	return b.trading.GetClock()
}

type AccountSnapshot struct {
	Equity      float64
	Cash        float64
	BuyingPower float64
}

func (b *Broker) GetAccount() (AccountSnapshot, error) {
	acc, err := b.trading.GetAccount()
	if err != nil {
		return AccountSnapshot{}, err
	}
	equity, _ := acc.Equity.Float64()
	cash, _ := acc.Cash.Float64()
	bp, _ := acc.BuyingPower.Float64()
	return AccountSnapshot{Equity: equity, Cash: cash, BuyingPower: bp}, nil
}

// GetBarsRange returns completed bars of the given timeframe for symbol
// between start and end (inclusive), oldest first. Used by the backtester
// for arbitrary historical windows.
func (b *Broker) GetBarsRange(symbol string, tf timeframe.Timeframe, start, end time.Time) ([]bar.Bar, error) {
	bars, err := b.data.GetBars(symbol, marketdata.GetBarsRequest{
		TimeFrame:  marketdata.NewTimeFrame(tf.N, marketdata.TimeFrameUnit(tf.Unit)),
		Start:      start,
		End:        end,
		Feed:       marketdata.IEX,
		Adjustment: marketdata.Raw,
	})
	if err != nil {
		return nil, fmt.Errorf("get %s bars for %s: %w", tf, symbol, err)
	}
	out := make([]bar.Bar, len(bars))
	for i, x := range bars {
		out[i] = bar.Bar{
			Time:   x.Timestamp,
			Open:   x.Open,
			High:   x.High,
			Low:    x.Low,
			Close:  x.Close,
			Volume: float64(x.Volume),
		}
	}
	return out, nil
}

// GetRecentBars returns the last lookbackDays worth of completed bars of the
// given timeframe for symbol, oldest first. Used by the live engine, which
// always wants "up through now".
func (b *Broker) GetRecentBars(symbol string, tf timeframe.Timeframe, lookbackDays int) ([]bar.Bar, error) {
	end := time.Now()
	start := end.AddDate(0, 0, -lookbackDays)
	return b.GetBarsRange(symbol, tf, start, end)
}

func (b *Broker) GetLatestPrice(symbol string) (float64, error) {
	t, err := b.data.GetLatestTrade(symbol, marketdata.GetLatestTradeRequest{Feed: marketdata.IEX})
	if err != nil {
		return 0, fmt.Errorf("get latest trade for %s: %w", symbol, err)
	}
	return t.Price, nil
}

// OpenPositionSymbols returns the set of symbols currently held.
func (b *Broker) OpenPositionSymbols() (map[string]bool, error) {
	positions, err := b.trading.GetPositions()
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(positions))
	for _, p := range positions {
		out[p.Symbol] = true
	}
	return out, nil
}

type HeldPosition struct {
	Symbol        string
	Qty           float64
	AvgEntryPrice float64
}

// GetOpenPositions returns every currently held position, regardless of
// whether this bot was the one that opened it.
func (b *Broker) GetOpenPositions() ([]HeldPosition, error) {
	positions, err := b.trading.GetPositions()
	if err != nil {
		return nil, err
	}
	out := make([]HeldPosition, 0, len(positions))
	for _, p := range positions {
		qty, _ := p.Qty.Float64()
		avgEntry, _ := p.AvgEntryPrice.Float64()
		out = append(out, HeldPosition{Symbol: p.Symbol, Qty: qty, AvgEntryPrice: avgEntry})
	}
	return out, nil
}

// HasLiveProtectiveStop reports, for each of the given symbols, whether it
// has an unresolved sell-side stop (or stop-limit) order — i.e. an actual
// stop-loss protecting the position, not just any open order. Alpaca's
// "status=open" shorthand excludes contingent bracket/OCO legs sitting in a
// "held" state, so this asks for recent order history instead and checks the
// lifecycle timestamps directly.
func (b *Broker) HasLiveProtectiveStop(symbols []string) (map[string]bool, error) {
	if len(symbols) == 0 {
		return map[string]bool{}, nil
	}
	orders, err := b.trading.GetOrders(alpaca.GetOrdersRequest{
		Status:    "all",
		Symbols:   symbols,
		Limit:     100,
		Direction: "desc",
	})
	if err != nil {
		return nil, err
	}
	protected := make(map[string]bool, len(symbols))
	for _, o := range orders {
		if o.Side != alpaca.Sell {
			continue
		}
		if o.Type != alpaca.Stop && o.Type != alpaca.StopLimit {
			continue
		}
		resolved := o.FilledAt != nil || o.ExpiredAt != nil || o.CanceledAt != nil || o.FailedAt != nil
		if !resolved {
			protected[o.Symbol] = true
		}
	}
	return protected, nil
}

// OpenOrderSymbols returns the set of symbols with a currently open (unfilled) order.
func (b *Broker) OpenOrderSymbols() (map[string]bool, error) {
	orders, err := b.trading.GetOrders(alpaca.GetOrdersRequest{Status: "open"})
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(orders))
	for _, o := range orders {
		out[o.Symbol] = true
	}
	return out, nil
}

// PlaceBracketBuy submits a market buy with an attached stop-loss and take-profit
// (a "bracket" order), so the exit is enforced server-side by Alpaca even if
// this bot is offline.
func (b *Broker) PlaceBracketBuy(symbol string, qty int64, stopPrice, takePrice float64) (*alpaca.Order, error) {
	q := decimal.NewFromInt(qty)
	// Both legs are sells (closing the long), and Alpaca rejects prices that
	// violate the minimum price variance (sub-penny above $1, sub-tenth-cent
	// below). Round to comply, floor-rounding as alpaca.RoundLimitPrice does
	// for sells so the exit is never worse than intended.
	stop := *alpaca.RoundLimitPrice(decimal.NewFromFloat(stopPrice), alpaca.Sell)
	take := *alpaca.RoundLimitPrice(decimal.NewFromFloat(takePrice), alpaca.Sell)

	return b.trading.PlaceOrder(alpaca.PlaceOrderRequest{
		Symbol:      symbol,
		Qty:         &q,
		Side:        alpaca.Buy,
		Type:        alpaca.Market,
		TimeInForce: alpaca.GTC,
		OrderClass:  alpaca.Bracket,
		TakeProfit:  &alpaca.TakeProfit{LimitPrice: &take},
		StopLoss:    &alpaca.StopLoss{StopPrice: &stop},
	})
}

type Fill struct {
	ID      string
	Time    time.Time
	Symbol  string
	Side    string
	Qty     float64
	Price   float64
	OrderID string
}

// GetFillActivities returns every order fill (partial or complete) strictly
// after the given time, oldest first. Alpaca's account-activity ledger is the
// authoritative trade history — it records fills even ones that happened
// while this bot wasn't running to see the order status change itself.
func (b *Broker) GetFillActivities(after time.Time) ([]Fill, error) {
	activities, err := b.trading.GetAccountActivities(alpaca.GetAccountActivitiesRequest{
		ActivityTypes: []string{"FILL"},
		After:         after,
		Direction:     "asc",
	})
	if err != nil {
		return nil, fmt.Errorf("get account activities: %w", err)
	}
	out := make([]Fill, 0, len(activities))
	for _, a := range activities {
		price, _ := a.Price.Float64()
		qty, _ := a.Qty.Float64()
		out = append(out, Fill{
			ID:      a.ID,
			Time:    a.TransactionTime,
			Symbol:  a.Symbol,
			Side:    a.Side,
			Qty:     qty,
			Price:   price,
			OrderID: a.OrderID,
		})
	}
	return out, nil
}

// PlaceProtectiveOCO attaches a stop-loss/take-profit pair to an EXISTING
// long position (no entry leg), for positions this bot didn't itself open
// with PlaceBracketBuy — e.g. opened manually, or left naked after a bracket
// leg was cancelled out from under it.
func (b *Broker) PlaceProtectiveOCO(symbol string, qty, stopPrice, takePrice float64) (*alpaca.Order, error) {
	q := decimal.NewFromFloat(qty)
	stop := *alpaca.RoundLimitPrice(decimal.NewFromFloat(stopPrice), alpaca.Sell)
	take := *alpaca.RoundLimitPrice(decimal.NewFromFloat(takePrice), alpaca.Sell)

	return b.trading.PlaceOrder(alpaca.PlaceOrderRequest{
		Symbol:      symbol,
		Qty:         &q,
		Side:        alpaca.Sell,
		Type:        alpaca.Limit,
		TimeInForce: alpaca.GTC,
		OrderClass:  alpaca.OCO,
		TakeProfit:  &alpaca.TakeProfit{LimitPrice: &take},
		StopLoss:    &alpaca.StopLoss{StopPrice: &stop},
	})
}

// ClosePosition liquidates the full position in symbol at market.
func (b *Broker) ClosePosition(symbol string) error {
	_, err := b.trading.ClosePosition(symbol, alpaca.ClosePositionRequest{})
	return err
}

// CancelAllOrders cancels every open order (used by the daily-loss kill switch).
func (b *Broker) CancelAllOrders() error {
	return b.trading.CancelAllOrders()
}
