package pay_test

import (
	"errors"
	"testing"
	"time"

	"github.com/lemmego/pay"
	"github.com/lemmego/pay/paytest"
)

func usd(amount int64) pay.Money { return pay.Money{Amount: amount, Currency: "usd"} }

func TestMonthlyRevenueNormalisesIntervals(t *testing.T) {
	cases := []struct {
		name string
		sub  pay.Subscription
		want int64
	}{
		{
			name: "monthly",
			sub:  pay.Subscription{Status: pay.StatusActive, UnitAmount: usd(1900), Interval: pay.Monthly, IntervalCount: 1, Quantity: 1},
			want: 1900,
		},
		{
			name: "yearly divides by twelve",
			sub:  pay.Subscription{Status: pay.StatusActive, UnitAmount: usd(4999), Interval: pay.Yearly, IntervalCount: 1, Quantity: 1},
			want: 417,
		},
		{
			name: "quarterly",
			sub:  pay.Subscription{Status: pay.StatusActive, UnitAmount: usd(3000), Interval: pay.Monthly, IntervalCount: 3, Quantity: 1},
			want: 1000,
		},
		{
			name: "weekly annualises then divides",
			sub:  pay.Subscription{Status: pay.StatusActive, UnitAmount: usd(500), Interval: pay.Weekly, IntervalCount: 1, Quantity: 1},
			want: 2167,
		},
		{
			name: "daily annualises then divides",
			sub:  pay.Subscription{Status: pay.StatusActive, UnitAmount: usd(100), Interval: pay.Daily, IntervalCount: 1, Quantity: 1},
			want: 3042,
		},
		{
			name: "per seat",
			sub:  pay.Subscription{Status: pay.StatusActive, UnitAmount: usd(1000), Interval: pay.Monthly, IntervalCount: 1, Quantity: 7},
			want: 7000,
		},
		{
			name: "a missing quantity is one, not none",
			sub:  pay.Subscription{Status: pay.StatusActive, UnitAmount: usd(1000), Interval: pay.Monthly, IntervalCount: 1},
			want: 1000,
		},
		{
			name: "a missing interval count is one",
			sub:  pay.Subscription{Status: pay.StatusActive, UnitAmount: usd(1000), Interval: pay.Monthly, Quantity: 1},
			want: 1000,
		},
		{
			name: "a percentage coupon",
			sub: pay.Subscription{Status: pay.StatusActive, UnitAmount: usd(2000), Interval: pay.Monthly,
				IntervalCount: 1, Quantity: 1, DiscountPercent: 25},
			want: 1500,
		},
		{
			name: "an amount coupon",
			sub: pay.Subscription{Status: pay.StatusActive, UnitAmount: usd(2000), Interval: pay.Monthly,
				IntervalCount: 1, Quantity: 1, DiscountAmount: usd(500)},
			want: 1500,
		},
		{
			name: "a coupon bigger than the plan",
			sub: pay.Subscription{Status: pay.StatusActive, UnitAmount: usd(500), Interval: pay.Monthly,
				IntervalCount: 1, Quantity: 1, DiscountAmount: usd(900)},
			want: 0,
		},
		{
			name: "an interval we do not know contributes nothing",
			sub:  pay.Subscription{Status: pay.StatusActive, UnitAmount: usd(1000), Interval: "fortnight", IntervalCount: 1, Quantity: 1},
			want: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.sub.MonthlyRevenue()
			if got.Amount != tc.want {
				t.Errorf("monthly revenue = %s, want %d", got, tc.want)
			}
		})
	}
}

// TestWhichStatusesEarnAndWhichEntitle pins the two lists apart. They differ
// in both directions and the difference is the product decision, not an
// oversight.
func TestWhichStatusesEarnAndWhichEntitle(t *testing.T) {
	cases := []struct {
		status            pay.Status
		earning, entitles bool
	}{
		{pay.StatusActive, true, true},
		// A trial entitles — the person is using the product — and earns
		// nothing, because nobody has paid yet.
		{pay.StatusTrialing, false, true},
		// A failed card still earns: the subscription exists and Stripe is
		// retrying. It stops entitling, because dunning runs for weeks.
		{pay.StatusPastDue, true, false},
		{pay.StatusUnpaid, false, false},
		{pay.StatusIncomplete, false, false},
		{pay.StatusIncompleteExpired, false, false},
		{pay.StatusPaused, false, false},
		{pay.StatusCanceled, false, false},
	}
	for _, tc := range cases {
		if got := tc.status.Earning(); got != tc.earning {
			t.Errorf("%s: Earning = %v, want %v", tc.status, got, tc.earning)
		}
		if got := tc.status.Entitling(); got != tc.entitles {
			t.Errorf("%s: Entitling = %v, want %v", tc.status, got, tc.entitles)
		}
	}

	// A trial contributes nothing to a revenue total, which is the whole
	// reason the two lists are separate.
	trial := pay.Subscription{Status: pay.StatusTrialing, UnitAmount: usd(9900),
		Interval: pay.Monthly, IntervalCount: 1, Quantity: 1}
	if got := trial.MonthlyRevenue(); !got.Zero() {
		t.Errorf("a trial contributed %s", got)
	}
}

func TestRevenueKeepsCurrenciesApart(t *testing.T) {
	revenue := pay.NewRevenue(time.Now())
	revenue.Add(&pay.Subscription{Status: pay.StatusActive, UnitAmount: usd(1000), Interval: pay.Monthly, Quantity: 1})
	revenue.Add(&pay.Subscription{Status: pay.StatusActive, UnitAmount: usd(500), Interval: pay.Monthly, Quantity: 1})
	revenue.Add(&pay.Subscription{Status: pay.StatusActive,
		UnitAmount: pay.Money{Amount: 2000, Currency: "eur"}, Interval: pay.Monthly, Quantity: 1})

	if got := revenue.In("usd").Amount; got != 1500 {
		t.Errorf("usd total = %d, want 1500", got)
	}
	if got := revenue.In("eur").Amount; got != 2000 {
		t.Errorf("eur total = %d, want 2000", got)
	}
	if revenue.Counted != 3 {
		t.Errorf("counted %d, want 3", revenue.Counted)
	}

	// Two true numbers and no single one. Summing them needs a rate, so the
	// badge refuses instead of inventing one.
	if _, err := revenue.Single(); !errors.Is(err, pay.ErrMixedCurrency) {
		t.Fatalf("Single on a two-currency account gave %v", err)
	}
}

func TestRevenueSkipsWhatItCannotCount(t *testing.T) {
	revenue := pay.NewRevenue(time.Now())
	// A multi-line subscription: the gateway reports only the first line, so
	// counting it would under-report by the rest.
	revenue.Add(&pay.Subscription{Status: pay.StatusActive, UnitAmount: usd(1000),
		Interval: pay.Monthly, Quantity: 1, MultipleItems: true})
	revenue.Add(&pay.Subscription{Status: pay.StatusCanceled, UnitAmount: usd(1000),
		Interval: pay.Monthly, Quantity: 1})
	revenue.Add(&pay.Subscription{Status: pay.StatusActive, UnitAmount: usd(1000),
		Interval: pay.Monthly, Quantity: 1})

	if revenue.Counted != 1 || revenue.Skipped != 2 {
		t.Errorf("counted %d skipped %d, want 1 and 2", revenue.Counted, revenue.Skipped)
	}
	// Reported rather than hidden, so a founder looking at a figure lower
	// than they expected can be told why.
	if got := revenue.In("usd").Amount; got != 1000 {
		t.Errorf("total = %d, want 1000", got)
	}
}

func TestSingleOnAnEmptyAccountIsNotAnError(t *testing.T) {
	revenue := pay.NewRevenue(time.Now())
	money, err := revenue.Single()
	if err != nil {
		t.Fatalf("an empty account: %v", err)
	}
	if !money.Zero() {
		t.Errorf("got %s", money)
	}
	// And no band: "no revenue verified" is a different claim from "under
	// $100", and only one of them is about money.
	if _, ok := pay.BandFor(money); ok {
		t.Error("zero revenue got a band")
	}
}

func TestTotalRefusesAPartialAnswer(t *testing.T) {
	reader := paytest.NewReader()
	many := make([]*pay.Subscription, pay.MaxAccountSubscriptions+1)
	for i := range many {
		many[i] = &pay.Subscription{Status: pay.StatusActive, UnitAmount: usd(100),
			Interval: pay.Monthly, Quantity: 1}
	}
	reader.Accounts["acct_big"] = many

	// Not a truncated sum. A figure computed from the first ten thousand
	// subscriptions, displayed as verified, would be wrong in the direction
	// that penalises the most successful account on the platform.
	if _, err := pay.Total(nil, reader, "acct_big", time.Now()); !errors.Is(err, pay.ErrTooManyRecords) {
		t.Fatalf("got %v, want ErrTooManyRecords", err)
	}

	reader.Accounts["acct_small"] = many[:3]
	revenue, err := pay.Total(nil, reader, "acct_small", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := revenue.In("usd").Amount; got != 300 {
		t.Errorf("total = %d, want 300", got)
	}
}

func TestBandFor(t *testing.T) {
	cases := []struct {
		money pay.Money
		label string
		ok    bool
	}{
		{usd(99), "", false}, // under a dollar a month in whole units? no — 99c
		{usd(100), "under $100/mo", true},
		{usd(9999), "under $100/mo", true},
		{usd(10000), "$100–1k/mo", true},
		{usd(499999), "$1k–5k/mo", true},
		{usd(500000), "$5k–10k/mo", true},
		{usd(10000000), "$100k+/mo", true},
		{usd(0), "", false},
		{usd(-500), "", false},
		// A currency the dollar ladder is not meaningful in. ¥10,000/mo is
		// about $65; putting it in the "$10k–50k" band would be a
		// two-orders-of-magnitude lie, so there is no band at all.
		{pay.Money{Amount: 10000, Currency: "jpy"}, "", false},
		{pay.Money{Amount: 10000, Currency: "eur"}, "$100–1k/mo", true},
	}
	for _, tc := range cases {
		band, ok := pay.BandFor(tc.money)
		if ok != tc.ok {
			t.Errorf("%s: ok = %v, want %v", tc.money, ok, tc.ok)
			continue
		}
		if ok && band.Label != tc.label {
			t.Errorf("%s: band = %q, want %q", tc.money, band.Label, tc.label)
		}
	}

	// Every band's rank round-trips, so a row read back renders the label it
	// was written with rather than one position off.
	for _, band := range pay.Bands() {
		got, ok := pay.BandByRank(band.Rank)
		if !ok || got.Label != band.Label {
			t.Errorf("rank %d did not round-trip", band.Rank)
		}
	}
}
