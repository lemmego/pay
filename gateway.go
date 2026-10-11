package pay

import (
	"context"
	"errors"
	"time"
)

// Errors a gateway returns. The application distinguishes them to decide
// whether a webhook delivery should be retried, which is the difference
// between losing a payment and not.
var (
	// ErrNoGateway means nothing is configured. Returned rather than panicked
	// so a development boot with no Stripe keys still runs; every paid path
	// then refuses with a message instead of the process dying.
	ErrNoGateway = errors.New("pay: no gateway is configured")
	// ErrNoStore means no persistence is wired, so a verified webhook would
	// be thrown away. It is a 500, because the alternative is telling Stripe
	// the event was handled when nothing recorded it.
	ErrNoStore = errors.New("pay: no store is configured")
	// ErrBadSignature means the delivery did not come from the gateway, or
	// came too long ago. Never retried and never applied.
	ErrBadSignature = errors.New("pay: the webhook signature does not verify")
	// ErrUnsupported is what a gateway returns for a capability it does not
	// have, so a caller can tell "this processor cannot do that" from "that
	// failed".
	ErrUnsupported = errors.New("pay: the gateway does not support this")
	// ErrTooManyRecords means a listing was longer than the cap. It exists so
	// a revenue figure is never computed from a partial list.
	ErrTooManyRecords = errors.New("pay: more records than the configured cap")
)

// Status is a subscription's lifecycle state, named as Stripe names them.
//
// Borrowing the vocabulary rather than inventing one is deliberate: a support
// conversation about somebody's billing has the dashboard open on one side,
// and a status of our own invention would need translating in both directions
// by whoever is least able to afford a mistake.
type Status string

const (
	StatusActive            Status = "active"
	StatusTrialing          Status = "trialing"
	StatusPastDue           Status = "past_due"
	StatusIncomplete        Status = "incomplete"
	StatusIncompleteExpired Status = "incomplete_expired"
	StatusUnpaid            Status = "unpaid"
	StatusPaused            Status = "paused"
	StatusCanceled          Status = "canceled"
)

// Entitling reports whether this status should unlock paid features.
//
// active and trialing, and nothing else. past_due is the interesting
// exclusion: the subscription still exists and Stripe is still retrying the
// card, so it counts as revenue (see Earning) — but a product that keeps
// serving paid features through an indefinite dunning cycle has made the
// payment optional. Stripe's retry schedule runs for weeks; GracePeriod is
// the explicit, short answer to "how long after a failure" instead.
func (s Status) Entitling() bool {
	return s == StatusActive || s == StatusTrialing
}

// Earning reports whether this status represents money the business is
// contracted to receive, for revenue reporting.
//
// It differs from Entitling in both directions, which is the point:
//
//   - trialing entitles but does not earn. A free trial is worth nothing this
//     month, and counting its list price is how a verified MRR badge becomes
//     a number the founder cannot reproduce from their own dashboard.
//   - past_due earns but does not entitle. The card failed; the subscription
//     has not ended, and Stripe is still trying.
func (s Status) Earning() bool {
	return s == StatusActive || s == StatusPastDue
}

// Interval is a billing period's unit.
type Interval string

const (
	Daily   Interval = "day"
	Weekly  Interval = "week"
	Monthly Interval = "month"
	Yearly  Interval = "year"
)

// Subscription is a recurring charge, flattened to what this module needs.
//
// Flattened deliberately: Stripe's subscription object has a list of items,
// each with its own price, quantity and interval, and modelling that
// faithfully means every caller handles the general case to serve the one
// plan this product sells. A gateway collapses it to one line and reports
// MultipleItems when it could not, so a caller that must be exact can tell.
type Subscription struct {
	ID         string `json:"id"`
	CustomerID string `json:"customer_id"`
	Status     Status `json:"status"`
	PriceID    string `json:"price_id"`

	// UnitAmount is the price of one unit for one interval, before discount.
	UnitAmount    Money    `json:"unit_amount"`
	Interval      Interval `json:"interval"`
	IntervalCount int64    `json:"interval_count"`
	Quantity      int64    `json:"quantity"`

	// DiscountPercent and DiscountAmount are the subscription-level coupon.
	// Only one of the two is ever set, which is how Stripe models a coupon.
	DiscountPercent float64 `json:"discount_percent"`
	DiscountAmount  Money   `json:"discount_amount"`

	// MultipleItems is set when the subscription had more than one line and
	// the fields above describe only the first. A revenue total refuses such
	// a subscription rather than under-reporting it.
	MultipleItems bool `json:"multiple_items"`

	TrialEndsAt       *time.Time `json:"trial_ends_at"`
	CurrentPeriodEnd  *time.Time `json:"current_period_end"`
	CancelAtPeriodEnd bool       `json:"cancel_at_period_end"`
	CanceledAt        *time.Time `json:"canceled_at"`
}

// MonthlyRevenue is this subscription's contribution to MRR.
//
// Zero for a status that does not earn, so a caller can sum unconditionally.
//
// Normalising a yearly plan to a month means dividing by twelve, which is a
// convention and not a fact: a month is not a twelfth of a year in days, and
// a weekly plan does not bill four times a month. The conventions used here
// are the ones every SaaS metrics tool uses — annualise, then divide by
// twelve — and they are written down here so the number on the badge can be
// reproduced by hand:
//
//	month  → amount / count
//	year   → amount / (12 × count)
//	week   → amount × 52 / (12 × count)
//	day    → amount × 365 / (12 × count)
//
// A leap year and a 53-week year are both ignored. They move the figure by
// well under the rounding of the band it is displayed in.
func (s *Subscription) MonthlyRevenue() Money {
	if !s.Status.Earning() {
		return Money{Currency: s.UnitAmount.Currency}
	}

	quantity := s.Quantity
	if quantity <= 0 {
		// Stripe omits quantity for a metered or a licensed-single price.
		// Treating a missing quantity as zero would silently erase the
		// subscription from the total.
		quantity = 1
	}
	gross := s.UnitAmount.Mul(quantity)

	switch {
	case s.DiscountPercent > 0:
		off := gross.Percent(s.DiscountPercent)
		discounted, err := gross.Sub(off)
		if err == nil {
			gross = discounted
		}
	case !s.DiscountAmount.Zero():
		// An amount_off coupon is per invoice, so it applies once per
		// interval — the same cadence as the amount it is subtracted from.
		discounted, err := gross.Sub(s.DiscountAmount)
		if err == nil {
			gross = discounted
		}
	}

	count := s.IntervalCount
	if count <= 0 {
		count = 1
	}

	switch s.Interval {
	case Monthly:
		return gross.DivRound(count)
	case Yearly:
		return gross.DivRound(12 * count)
	case Weekly:
		return gross.Mul(52).DivRound(12 * count)
	case Daily:
		return gross.Mul(365).DivRound(12 * count)
	default:
		// An interval we do not recognise contributes nothing rather than
		// being guessed at. A wrong figure on a badge that says "verified" is
		// worse than a missing one.
		return Money{Currency: s.UnitAmount.Currency}
	}
}

// EntitledUntil is when paid features should stop, absent another event.
//
// The current period's end, not the cancellation date: somebody who cancels
// on the 2nd has paid through the month and keeps what they bought. A
// subscription with no period end — which Stripe only reports for one that
// never started — entitles nobody.
func (s *Subscription) EntitledUntil() *time.Time {
	if !s.Status.Entitling() {
		return nil
	}
	return s.CurrentPeriodEnd
}

// CheckoutRequest is a hosted purchase about to be started.
type CheckoutRequest struct {
	// PriceID is the gateway's identifier for what is being sold. Prices live
	// in the gateway, not in this application: a price duplicated into a
	// local table is a price that will eventually disagree with the one the
	// customer is actually charged.
	PriceID string
	// CustomerID reuses an existing customer when there is one, so a second
	// subscription does not create a second customer record for one person.
	CustomerID string
	// Email prefills the form for somebody who has no customer record yet.
	Email string
	// ClientReference is this application's own id for the buyer. It comes
	// back on the webhook, and it is the only thing that does: a checkout
	// session knows nothing about users, so without it a completed payment
	// cannot be attributed to anybody.
	ClientReference string
	// SuccessURL and CancelURL are where the gateway returns the browser.
	SuccessURL string
	CancelURL  string
	// TrialDays starts the subscription in a trial.
	TrialDays int
	// IdempotencyKey makes a retried create return the first session rather
	// than opening a second one. A double-submitted form otherwise bills
	// twice.
	IdempotencyKey string
}

// Checkout is a started hosted purchase.
type Checkout struct {
	ID  string
	URL string
}

// Price is what something costs, as the gateway holds it.
//
// Read from the gateway and never stored here. A price copied into this
// application's configuration is a price that will eventually disagree with
// the one the customer is actually charged — and the page that disagrees is
// the pitch page, which is the worst possible place to be wrong about a
// number.
type Price struct {
	ID            string   `json:"id"`
	Amount        Money    `json:"amount"`
	Interval      Interval `json:"interval"`
	IntervalCount int64    `json:"interval_count"`
}

// Gateway is one payment processor.
//
// Deliberately small. Everything here is something this product does: start a
// purchase, let somebody manage what they bought, read back a subscription,
// and authenticate a webhook. Refunds, invoices, metered usage, tax and
// one-off charges are absent because adding a method per Stripe endpoint
// produces an interface only Stripe can implement, which is the opposite of
// why it exists.
type Gateway interface {
	// Name identifies the gateway in stored rows, so a row written by one
	// processor is not read as another's.
	Name() string

	// Checkout starts a hosted subscription purchase.
	Checkout(ctx context.Context, req CheckoutRequest) (*Checkout, error)

	// Portal returns a URL where a customer manages their own subscription.
	//
	// Hosted rather than built here, and that is a decision worth stating: a
	// cancel button of our own means handling proration, payment-method
	// updates, failed-card recovery and tax-id changes, and getting any of
	// them subtly wrong is a chargeback.
	Portal(ctx context.Context, customerID, returnURL string) (string, error)

	// Subscription reads one subscription's current state.
	Subscription(ctx context.Context, id string) (*Subscription, error)

	// Price reads what something costs.
	//
	// Here rather than in configuration so a page shows the amount the
	// customer will actually be charged. Callers are expected to cache it:
	// a price changes when somebody edits it in a dashboard, which is rarely,
	// and a pitch page must not make a network call per view.
	Price(ctx context.Context, id string) (*Price, error)

	// Verify authenticates a webhook delivery and returns the event it
	// carries. payload must be the exact bytes received.
	Verify(payload []byte, signature string, now time.Time) (*Event, error)
}

// RevenueReader reads the subscriptions of an account that is not ours.
//
// Separate from Gateway, and separately configured, because it is a different
// thing wearing the same brand name. Charging somebody for Pro uses our own
// secret key; reading their revenue to verify a badge uses an authorisation
// they granted over their account, and the credential, the permission and the
// blast radius are all different. A single interface holding both would mean
// every gateway implementation had to be trusted with both.
type RevenueReader interface {
	// Subscriptions calls each with every subscription on the account, paging
	// as needed, and returns ErrTooManyRecords rather than stopping early.
	//
	// A callback rather than a slice: an account can have tens of thousands
	// of subscriptions, and the caller only ever needs a running total.
	Subscriptions(ctx context.Context, account string, each func(*Subscription) error) error

	// Account reports whether an account authorisation is still good, so a
	// badge can be withdrawn when access is revoked rather than showing a
	// figure nobody can check any more.
	Account(ctx context.Context, account string) error
}

// Authorizer is a RevenueReader whose access the account owner grants through
// a redirect flow.
//
// A second interface rather than two more methods on RevenueReader, because
// not every way of reading an account involves one — an API key the founder
// pastes in needs no redirect, and an internal reader needs no grant at all.
// A caller asks for what it needs:
//
//	if authorizer, ok := reader.(pay.Authorizer); ok { … }
//
// It exists because the alternative is what this was first written as: the
// handler asserted the concrete Stripe type. That compiles, works, and means
// no other implementation — a second processor, or a test fake — can ever
// drive the flow, which is the whole thing an interface was for.
type Authorizer interface {
	// AuthorizeURL is where the account owner is sent to grant access.
	//
	// state must be bound to the session that started the flow. An
	// implementation refuses an empty one rather than inventing a value,
	// because a state bound to nothing is a CSRF hole wearing the name of
	// its own defence.
	AuthorizeURL(state, redirectURI string) (string, error)

	// Exchange turns the code from the redirect into an account identifier.
	Exchange(ctx context.Context, code string) (string, error)
}
