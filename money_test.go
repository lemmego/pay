package pay_test

import (
	"errors"
	"testing"

	"github.com/lemmego/pay"
)

// TestZeroDecimalCurrenciesAreNotDividedByAHundred is the test for the
// mistake that is easiest to make and hardest to notice: Stripe reports a
// ¥5,000 subscription as 5000, not 500000.
func TestZeroDecimalCurrenciesAreNotDividedByAHundred(t *testing.T) {
	cases := []struct {
		money pay.Money
		major int64
	}{
		{pay.Money{Amount: 4999, Currency: "usd"}, 49},
		{pay.Money{Amount: 5000, Currency: "jpy"}, 5000},
		{pay.Money{Amount: 5000, Currency: "krw"}, 5000},
		{pay.Money{Amount: 5000, Currency: "JPY"}, 5000},
		// Three-decimal: a 5.000 dinar price is reported as 5000.
		{pay.Money{Amount: 5000, Currency: "kwd"}, 5},
	}
	for _, tc := range cases {
		if got := tc.money.Major(); got != tc.major {
			t.Errorf("%s: major = %d, want %d", tc.money, got, tc.major)
		}
	}
}

func TestDivRoundRoundsHalfAway(t *testing.T) {
	cases := []struct {
		amount  int64
		divisor int64
		want    int64
	}{
		// $49.99/year over twelve months. Truncation gives 416, which
		// under-reports every yearly subscriber by a cent a month.
		{4999, 12, 417},
		{100, 3, 33},
		{200, 3, 67},
		{1, 2, 1},
		{-1, 2, -1},
		{0, 12, 0},
	}
	for _, tc := range cases {
		got := pay.Money{Amount: tc.amount, Currency: "usd"}.DivRound(tc.divisor)
		if got.Amount != tc.want {
			t.Errorf("%d/%d = %d, want %d", tc.amount, tc.divisor, got.Amount, tc.want)
		}
	}

	// A zero divisor returns zero rather than panicking: the inputs come from
	// somebody else's API, and interval_count arriving as 0 should not take
	// the worker down.
	if got := (pay.Money{Amount: 100, Currency: "usd"}).DivRound(0); got.Amount != 0 {
		t.Errorf("dividing by zero gave %s", got)
	}
}

func TestAddRefusesMixedCurrencies(t *testing.T) {
	usd := pay.Money{Amount: 1000, Currency: "usd"}
	eur := pay.Money{Amount: 1000, Currency: "eur"}

	if _, err := usd.Add(eur); !errors.Is(err, pay.ErrMixedCurrency) {
		t.Fatalf("adding usd and eur gave %v, want ErrMixedCurrency", err)
	}

	// The zero value is additive identity, so a running total can start empty.
	sum, err := pay.Money{}.Add(usd)
	if err != nil || sum != usd {
		t.Fatalf("zero + usd = %s, %v", sum, err)
	}
	sum, err = usd.Add(pay.Money{})
	if err != nil || sum != usd {
		t.Fatalf("usd + zero = %s, %v", sum, err)
	}

	// Case does not make two currencies different.
	if _, err := usd.Add(pay.Money{Amount: 1, Currency: "USD"}); err != nil {
		t.Fatalf("adding usd and USD: %v", err)
	}
}

func TestSubFloorsAtZero(t *testing.T) {
	// A coupon worth more than the subscription is a free subscription, not a
	// negative one — a negative would silently reduce somebody else's total
	// in the same currency.
	got, err := pay.Money{Amount: 500, Currency: "usd"}.
		Sub(pay.Money{Amount: 900, Currency: "usd"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Amount != 0 {
		t.Errorf("500 - 900 = %s, want 0", got)
	}
}

func TestPercentRounds(t *testing.T) {
	// 33.33% of $49.99 is 1666.1667 cents.
	got := pay.Money{Amount: 4999, Currency: "usd"}.Percent(33.33)
	if got.Amount != 1666 {
		t.Errorf("33.33%% of 4999 = %s, want 1666", got)
	}
	if got := (pay.Money{Amount: 4999, Currency: "usd"}).Percent(0); got.Amount != 0 {
		t.Errorf("0%% = %s", got)
	}
}
