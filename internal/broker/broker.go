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

// GetDailyBars returns the last lookbackDays of completed daily bars for symbol,
// oldest first.
func (b *Broker) GetDailyBars(symbol string, lookbackDays int) ([]bar.Bar, error) {
	end := time.Now()
	start := end.AddDate(0, 0, -lookbackDays*2) // generous padding for weekends/holidays

	bars, err := b.data.GetBars(symbol, marketdata.GetBarsRequest{
		TimeFrame:  marketdata.OneDay,
		Start:      start,
		End:        end,
		Feed:       marketdata.IEX,
		Adjustment: marketdata.Raw,
	})
	if err != nil {
		return nil, fmt.Errorf("get bars for %s: %w", symbol, err)
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
	if len(out) > lookbackDays {
		out = out[len(out)-lookbackDays:]
	}
	return out, nil
}

// GetDailyBarsRange returns completed daily bars for symbol between start and
// end (inclusive), oldest first. Used by the backtester for arbitrary
// historical windows.
func (b *Broker) GetDailyBarsRange(symbol string, start, end time.Time) ([]bar.Bar, error) {
	bars, err := b.data.GetBars(symbol, marketdata.GetBarsRequest{
		TimeFrame:  marketdata.OneDay,
		Start:      start,
		End:        end,
		Feed:       marketdata.IEX,
		Adjustment: marketdata.Raw,
	})
	if err != nil {
		return nil, fmt.Errorf("get bars for %s: %w", symbol, err)
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

// ClosePosition liquidates the full position in symbol at market.
func (b *Broker) ClosePosition(symbol string) error {
	_, err := b.trading.ClosePosition(symbol, alpaca.ClosePositionRequest{})
	return err
}

// CancelAllOrders cancels every open order (used by the daily-loss kill switch).
func (b *Broker) CancelAllOrders() error {
	return b.trading.CancelAllOrders()
}
