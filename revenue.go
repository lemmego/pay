package pay

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// MaxAccountSubscriptions caps how many subscriptions a revenue total will
// read.
//
// The cap exists so a figure is never computed from part of the data. An
// account with more subscriptions than this gets ErrTooManyRecords and no
// badge, rather than a badge showing the first ten thousand subscriptions'
// worth of revenue — which would be a wrong number wearing the word
// "verified", and wrong in the direction that penalises the most successful
// account.
const MaxAccountSubscriptions = 10000

// Revenue is a monthly recurring total, kept per currency.
//
// Per currency and not summed, because summing needs an exchange rate. A
// founder billing in dollars and euros has two true numbers and no single one;
// inventing a third from a rate fetched this morning would make the badge
// depend on a fact that changes while nobody is looking, and the figure would
// stop matching their own dashboard.
type Revenue struct {
	byCurrency map[string]int64
	// Counted is how many subscriptions contributed, and Skipped how many were
	// read and did not — a trial, a cancellation, a multi-line subscription.
	// Both are reported so a founder looking at a figure lower than they
	// expected can be told why rather than left to guess.
	Counted int
	Skipped int
	// At is when the figures were read.
	At time.Time
}

// NewRevenue returns an empty total.
func NewRevenue(at time.Time) *Revenue {
	return &Revenue{byCurrency: map[string]int64{}, At: at}
}

// Add folds one subscription into the total.
func (r *Revenue) Add(s *Subscription) {
	if r.byCurrency == nil {
		r.byCurrency = map[string]int64{}
	}

	// A subscription with more than one line is skipped, not estimated. The
	// gateway reports only the first line's price, so counting it would
	// under-report by however many lines were dropped — and a founder whose
	// plans bundle a base fee with per-seat pricing is exactly the founder
	// who would notice.
	if s.MultipleItems {
		r.Skipped++
		return
	}

	monthly := s.MonthlyRevenue()
	if monthly.Zero() {
		r.Skipped++
		return
	}
	r.byCurrency[strings.ToLower(monthly.Currency)] += monthly.Amount
	r.Counted++
}

// Currencies lists the currencies with a non-zero total, in a stable order.
func (r *Revenue) Currencies() []string {
	codes := make([]string, 0, len(r.byCurrency))
	for code, amount := range r.byCurrency {
		if amount != 0 {
			codes = append(codes, code)
		}
	}
	sort.Strings(codes)
	return codes
}

// In returns the total for one currency.
func (r *Revenue) In(currency string) Money {
	code := strings.ToLower(currency)
	return Money{Amount: r.byCurrency[code], Currency: code}
}

// Single returns the one currency's total, or ErrMixedCurrency.
//
// A badge calls this. Refusing a multi-currency account is the honest
// outcome: there is no single figure to show, and showing one of the two as
// though it were the whole would under-report by exactly the part that was
// dropped.
func (r *Revenue) Single() (Money, error) {
	codes := r.Currencies()
	switch len(codes) {
	case 0:
		return Money{}, nil
	case 1:
		return r.In(codes[0]), nil
	default:
		return Money{}, fmt.Errorf("%w: %s", ErrMixedCurrency, strings.Join(codes, ", "))
	}
}

// Total reads every subscription on an account and returns its monthly
// recurring revenue.
func Total(ctx Context, reader RevenueReader, account string, now time.Time) (*Revenue, error) {
	if reader == nil {
		return nil, ErrUnsupported
	}
	revenue := NewRevenue(now)
	seen := 0
	err := reader.Subscriptions(ctx, account, func(s *Subscription) error {
		seen++
		if seen > MaxAccountSubscriptions {
			return ErrTooManyRecords
		}
		revenue.Add(s)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return revenue, nil
}

// ---------------------------------------------------------------------------
// Bands

// Band is a range of monthly revenue, in whole units of its currency.
//
// Revenue is published as a band and not as a figure, which is a product
// decision with a privacy reason behind it. An exact number is somebody's
// business performance, inferable back to their customer count once a plan
// price is public, and it invites the kind of competitive attention nobody
// signed up for. A band answers the question the reader actually has — is
// this person's thing working — and answers nothing else.
type Band struct {
	// Min is inclusive, Max exclusive. Max is zero on the open-ended band.
	Min   int64  `json:"min"`
	Max   int64  `json:"max"`
	Label string `json:"label"`
	// Rank orders bands without exposing the amount behind them.
	Rank int `json:"rank"`
}

// Open reports whether this is the top, unbounded band.
func (b Band) Open() bool { return b.Max == 0 }

// bands is the published ladder, in whole currency units per month.
//
// Deliberately coarse at the bottom and open at the top. The interesting
// distinction for a reader is the order of magnitude, and a ladder fine
// enough to separate $4,900 from $5,100 is a ladder that reveals the figure
// it was meant to hide.
var bands = []Band{
	{Min: 1, Max: 100, Label: "under $100/mo", Rank: 1},
	{Min: 100, Max: 1000, Label: "$100–1k/mo", Rank: 2},
	{Min: 1000, Max: 5000, Label: "$1k–5k/mo", Rank: 3},
	{Min: 5000, Max: 10000, Label: "$5k–10k/mo", Rank: 4},
	{Min: 10000, Max: 50000, Label: "$10k–50k/mo", Rank: 5},
	{Min: 50000, Max: 100000, Label: "$50k–100k/mo", Rank: 6},
	{Min: 100000, Max: 0, Label: "$100k+/mo", Rank: 7},
}

// bandedCurrencies are the currencies the ladder above is meaningful in.
//
// The thresholds are dollar amounts. Reusing them for a currency of a very
// different scale would put a ¥10,000/mo business — roughly $65 — in the
// "$10k–50k" band, which is not a rounding error but a two-orders-of-magnitude
// lie. Converting instead needs an exchange rate, which is the thing this
// module refuses to depend on.
//
// So an account billing in a currency that is not here is verified and shown
// without a band. Fewer claims, all of them true.
var bandedCurrencies = map[string]bool{
	"usd": true, "eur": true, "gbp": true, "cad": true, "aud": true,
	"chf": true, "nzd": true, "sgd": true,
}

// BandFor returns the band a monthly figure falls in.
//
// The second return is false when there is no band to show: a currency the
// ladder does not cover, or no revenue at all. A caller must not treat that
// as the bottom band — "no revenue verified" and "under $100" are different
// claims, and only one of them is about money.
func BandFor(monthly Money) (Band, bool) {
	if monthly.Amount <= 0 {
		return Band{}, false
	}
	if !bandedCurrencies[strings.ToLower(monthly.Currency)] {
		return Band{}, false
	}
	major := monthly.Major()
	if major < bands[0].Min {
		// Rounds down to nothing in whole units — a sub-$1/mo total. Showing
		// "under $100" for it would be true and useless; showing nothing is
		// the same information with less implication.
		return Band{}, false
	}
	for _, band := range bands {
		if band.Open() && major >= band.Min {
			return band, true
		}
		if major >= band.Min && major < band.Max {
			return band, true
		}
	}
	return Band{}, false
}

// Bands returns the published ladder, for a page that explains it.
func Bands() []Band { return append([]Band(nil), bands...) }

// BandByRank returns a band by its stored rank, so a row read back from the
// database renders the same label it was written with.
func BandByRank(rank int) (Band, bool) {
	for _, band := range bands {
		if band.Rank == rank {
			return band, true
		}
	}
	return Band{}, false
}
