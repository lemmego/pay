package pay

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ErrMixedCurrency is returned when amounts in different currencies would
// have to be added to answer the question.
//
// There is no exchange rate in this module and there should not be: a rate is
// a time-varying external fact, and a revenue figure that silently depends on
// one is not verifiable by the person reading it.
var ErrMixedCurrency = errors.New("pay: amounts are in more than one currency")

// Money is an amount in a currency's smallest unit.
//
// Integer minor units, never a float. A float64 holds 0.1 as
// 0.1000000000000000055511151231257827, and a sum of a few thousand
// subscription amounts drifts by enough to show the wrong figure — which for a
// badge that calls itself verified is the whole value gone. Stripe reports
// amounts this way for the same reason.
type Money struct {
	// Amount is in minor units — cents for usd, yen for jpy. See Factor.
	Amount int64 `json:"amount"`
	// Currency is a lowercase ISO 4217 code, as Stripe reports it.
	Currency string `json:"currency"`
}

// zeroDecimal are the currencies whose minor unit is the unit.
//
// This list is not a nicety. Stripe reports a ¥5,000 subscription as
// amount 5000, not 500000, so dividing by 100 to display it gives ¥50 — a
// figure wrong by two orders of magnitude, in the direction that makes a
// Japanese founder's revenue look like a rounding error. The set comes from
// Stripe's zero-decimal currency list.
var zeroDecimal = map[string]bool{
	"bif": true, "clp": true, "djf": true, "gnf": true, "jpy": true,
	"kmf": true, "krw": true, "mga": true, "pyg": true, "rwf": true,
	"ugx": true, "vnd": true, "vuv": true, "xaf": true, "xof": true,
	"xpf": true,
}

// threeDecimal currencies are quoted in thousandths. Stripe requires the
// amount to be an even multiple of ten for these, but it is still reported in
// thousandths, so the divisor is 1000.
var threeDecimal = map[string]bool{
	"bhd": true, "iqd": true, "jod": true, "kwd": true,
	"lyd": true, "omr": true, "tnd": true,
}

// Factor is how many minor units make one unit of the currency.
func Factor(currency string) int64 {
	switch c := strings.ToLower(currency); {
	case zeroDecimal[c]:
		return 1
	case threeDecimal[c]:
		return 1000
	default:
		return 100
	}
}

// Zero reports whether there is no money here.
func (m Money) Zero() bool { return m.Amount == 0 }

// Major is the amount in whole units, truncated.
//
// For display and for banding, never for arithmetic: two calls to Major and an
// addition lose the fractional part twice.
func (m Money) Major() int64 { return m.Amount / Factor(m.Currency) }

// Add returns the sum, refusing to add across currencies.
func (m Money) Add(other Money) (Money, error) {
	if m.Amount == 0 && m.Currency == "" {
		return other, nil
	}
	if other.Amount == 0 && other.Currency == "" {
		return m, nil
	}
	if !strings.EqualFold(m.Currency, other.Currency) {
		return Money{}, fmt.Errorf("%w: %s and %s", ErrMixedCurrency, m.Currency, other.Currency)
	}
	return Money{Amount: m.Amount + other.Amount, Currency: m.Currency}, nil
}

// Mul scales the amount by a whole number of units.
func (m Money) Mul(n int64) Money {
	return Money{Amount: m.Amount * n, Currency: m.Currency}
}

// DivRound divides the amount, rounding half away from zero.
//
// The rounding direction is stated rather than inherited. Go's integer
// division truncates toward zero, so a yearly plan normalised to a month by
// truncation under-reports every single time — eleven cents on a $49.99/year
// plan, which is small until it is a leaderboard and everyone is one band
// lower than they earned.
func (m Money) DivRound(divisor int64) Money {
	if divisor == 0 {
		return Money{Currency: m.Currency}
	}
	negative := (m.Amount < 0) != (divisor < 0)
	a, d := abs64(m.Amount), abs64(divisor)
	q := (a + d/2) / d
	if negative {
		q = -q
	}
	return Money{Amount: q, Currency: m.Currency}
}

// Percent returns the given percentage of the amount, rounded to the minor
// unit.
//
// The percentage is a float because Stripe's percent_off is one — a coupon can
// be 33.33% off. It is a multiplier passed straight through to a rounded
// integer result and is never stored: that is the only shape in which a float
// is allowed anywhere near an amount here.
func (m Money) Percent(pct float64) Money {
	if pct <= 0 {
		return Money{Currency: m.Currency}
	}
	scaled := math.Round(float64(m.Amount) * pct / 100)
	return Money{Amount: int64(scaled), Currency: m.Currency}
}

// Sub returns the difference, floored at zero.
//
// Floored because the only subtraction here is applying a discount, and a
// coupon worth more than the subscription is a free subscription, not a
// negative one. A negative amount flowing into an MRR total would quietly
// reduce somebody else's.
func (m Money) Sub(other Money) (Money, error) {
	if !strings.EqualFold(m.Currency, other.Currency) && other.Currency != "" {
		return Money{}, fmt.Errorf("%w: %s and %s", ErrMixedCurrency, m.Currency, other.Currency)
	}
	amount := m.Amount - other.Amount
	if amount < 0 {
		amount = 0
	}
	return Money{Amount: amount, Currency: m.Currency}, nil
}

// String formats the amount for a log line, not for a page.
//
// A page needs the reader's locale, the currency's symbol and its placement,
// none of which belong in a payments module. This is unambiguous instead:
// "4999 usd" cannot be mistaken for a localised figure the way "$49.99" can.
func (m Money) String() string {
	if m.Currency == "" {
		return strconv.FormatInt(m.Amount, 10)
	}
	return strconv.FormatInt(m.Amount, 10) + " " + strings.ToLower(m.Currency)
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
