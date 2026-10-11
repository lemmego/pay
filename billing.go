package pay

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/lemmego/api/app"
)

// MaxWebhookBody bounds what a webhook delivery may send.
//
// A megabyte is far larger than any event Stripe sends and far smaller than
// what an unbounded io.ReadAll on a public endpoint will eventually be asked
// to hold. The endpoint is unauthenticated until the signature has been
// checked, and the signature cannot be checked without reading the body — so
// anybody at all can make this process allocate, and the only question is how
// much.
const MaxWebhookBody = 1 << 20

// GracePeriod is how long a lapsed subscription keeps its features.
//
// Two days, for one reason: a renewal that succeeds a few hours late, or a
// webhook delivered after a brief outage, should not take Pro away from
// somebody who has paid. The alternative is a customer who watches their
// badge disappear and reappear, which costs more in support than two days of
// features cost in revenue.
//
// It is deliberately short. A long grace period is a free tier nobody decided
// to offer.
const GracePeriod = 48 * time.Hour

// Billing is the application's payment facade.
type Billing struct {
	gateway   Gateway
	revenue   RevenueReader
	store     Store
	secrets   []string
	tolerance time.Duration
	livemode  bool
}

// New returns a Billing. Most applications get one from Provider.
func New(gateway Gateway, store Store, opts ...Option) *Billing {
	b := &Billing{gateway: gateway, store: store, tolerance: DefaultTolerance}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Option configures a Billing.
type Option func(*Billing)

// WithSecrets sets the webhook signing secrets. More than one so a secret can
// be rotated without an interval of rejected events.
func WithSecrets(secrets ...string) Option {
	return func(b *Billing) { b.secrets = secrets }
}

// WithTolerance overrides how stale a delivery's signature may be.
func WithTolerance(d time.Duration) Option {
	return func(b *Billing) { b.tolerance = d }
}

// WithRevenueReader attaches the read-only revenue capability.
func WithRevenueReader(r RevenueReader) Option {
	return func(b *Billing) { b.revenue = r }
}

// WithLivemode declares which mode this process is in, so a delivery from the
// other one is refused.
//
// Without this check a test-mode event — which anybody with the publishable
// key can cause, and which the Stripe CLI produces on demand — lands on a
// production row and grants a subscription nobody paid for. The signature
// proves the event came from Stripe; it says nothing about which Stripe.
func WithLivemode(live bool) Option {
	return func(b *Billing) { b.livemode = live }
}

// Gateway returns the configured gateway, or nil.
func (b *Billing) Gateway() Gateway { return b.gateway }

// Revenue returns the configured revenue reader, and whether there is one.
func (b *Billing) Revenue() (RevenueReader, bool) {
	return b.revenue, b.revenue != nil
}

// Checkout starts a hosted purchase.
func (b *Billing) Checkout(ctx Context, req CheckoutRequest) (*Checkout, error) {
	if b.gateway == nil {
		return nil, ErrNoGateway
	}
	if req.PriceID == "" {
		return nil, errors.New("pay: Checkout needs a PriceID")
	}
	if req.ClientReference == "" {
		// Refused rather than defaulted. A checkout with no client reference
		// completes, takes the money, and arrives on the webhook attached to
		// nobody — recoverable only by hand, from the customer's email
		// address, by somebody who has noticed.
		return nil, errors.New("pay: Checkout needs a ClientReference to attribute the payment")
	}
	return b.gateway.Checkout(ctx, req)
}

// Portal returns the URL where a customer manages their subscription.
func (b *Billing) Portal(ctx Context, customerID, returnURL string) (string, error) {
	if b.gateway == nil {
		return "", ErrNoGateway
	}
	if customerID == "" {
		return "", errors.New("pay: Portal needs a customer")
	}
	return b.gateway.Portal(ctx, customerID, returnURL)
}

// Subscription reads one subscription from the gateway.
//
// For a reconciliation job, not for a page. A page reads the application's own
// row: the gateway is somebody else's service over a network, and a profile
// that cannot render while Stripe is slow is a profile that cannot render.
func (b *Billing) Subscription(ctx Context, id string) (*Subscription, error) {
	if b.gateway == nil {
		return nil, ErrNoGateway
	}
	return b.gateway.Subscription(ctx, id)
}

// Handle verifies a delivery and applies it.
//
// The order is the whole correctness of the thing: verify the signature
// before parsing anything, record the event id before applying it, and apply
// in one conditional write. Each step has a failure mode the next one cannot
// fix.
func (b *Billing) Handle(ctx Context, payload []byte, signature string) error {
	if b.gateway == nil {
		return ErrNoGateway
	}
	if b.store == nil {
		return ErrNoStore
	}

	if err := VerifySignature(payload, signature, b.secrets, time.Now(), b.tolerance); err != nil {
		return err
	}

	event, err := b.gateway.Verify(payload, signature, time.Now())
	if err != nil {
		return err
	}

	if event.Livemode != b.livemode {
		return fmt.Errorf("%w: a %s-mode event arrived at a %s-mode endpoint",
			ErrBadSignature, modeName(event.Livemode), modeName(b.livemode))
	}

	first, err := b.store.Seen(ctx, event)
	if err != nil {
		return err
	}
	if !first {
		// Already handled. Not an error: a retry of something that worked is
		// the system behaving correctly, and reporting it as a failure makes
		// Stripe retry it again.
		return nil
	}

	return b.store.Apply(ctx, event)
}

func modeName(live bool) string {
	if live {
		return "live"
	}
	return "test"
}

// WebhookHandler is the HTTP endpoint a gateway posts deliveries to.
//
// The status codes are the contract with the gateway's retry logic, and they
// are the part that is easy to get expensively wrong:
//
//   - 400 for a signature that does not verify. Stripe does not retry a 4xx,
//     which is what we want: a forged or stale delivery will not verify on
//     the tenth attempt either.
//   - 200 for an event we do not handle. Anything else makes Stripe retry an
//     event forever, and an endpoint that keeps failing gets disabled —
//     taking the events we do handle with it.
//   - 500 only when the store failed. That is the one case where retrying is
//     the right thing, because the event was real and nothing recorded it.
//
// Nothing here logs the payload or the signature header. The payload is
// somebody's billing details and the header is a credential-equivalent; a log
// line holding either turns log access into billing access.
func WebhookHandler() app.Handler {
	return func(c app.Context) error {
		billing, ok := Get(c.App())
		if !ok || billing == nil {
			slog.Error("pay: a webhook arrived but no billing is configured")
			return c.Error(http.StatusInternalServerError, ErrNoGateway)
		}

		// The body is read here, once, and passed through unmodified. The
		// signature covers these exact bytes, so anything that decodes and
		// re-encodes the JSON — a binding middleware, an input struct —
		// invalidates it.
		//
		// It survives this far because the method-override middleware calls
		// PostFormValue, which for a JSON content type parses nothing and
		// leaves the body alone. That is a thin guarantee to rest on, which
		// is why this reads the raw body itself rather than asking the
		// context for a parsed one.
		defer c.Request().Body.Close()
		payload, err := io.ReadAll(http.MaxBytesReader(
			c.ResponseWriter(), c.Request().Body, MaxWebhookBody))
		if err != nil {
			return c.BadRequest(errors.New("pay: the webhook body could not be read"))
		}

		switch err := billing.Handle(c.RequestContext(), payload, c.Header(SignatureHeader)); {
		case err == nil:
			return c.SetStatus(http.StatusOK).JSON(app.M{"received": true})

		case errors.Is(err, ErrBadSignature):
			// Logged without the header or the body. "A delivery did not
			// verify" is the whole actionable content; the rest is either
			// secret or attacker-controlled.
			slog.Warn("pay: a webhook delivery did not verify", "error", err)
			return c.Error(http.StatusBadRequest, errors.New("signature verification failed"))

		case errors.Is(err, ErrUnsupported):
			// An event type this application does not handle. Acknowledged,
			// so Stripe stops sending it.
			return c.SetStatus(http.StatusOK).JSON(app.M{"ignored": true})

		default:
			slog.Error("pay: a verified webhook could not be applied", "error", err)
			return c.Error(http.StatusInternalServerError, errors.New("the event could not be recorded"))
		}
	}
}

// ---------------------------------------------------------------------------
// Package facade

var global *Billing

// SetGlobal installs the Billing the package functions use. Provider calls it.
func SetGlobal(b *Billing) { global = b }

// Global returns the installed Billing, so a test can save and restore it.
func Global() *Billing { return global }

// Get returns the application's Billing.
func Get(a app.App) (*Billing, bool) {
	if b, ok := app.Lookup[*Billing](a); ok && b != nil {
		return b, true
	}
	return global, global != nil
}

// Price reads what something costs, from the gateway.
//
// Callers cache it. See handlers.proPrice in the application for the shape:
// one read an hour, and no price shown at all when the gateway is
// unreachable — because a stale figure on a pitch page is a figure that
// disagrees with the checkout the person is about to see.
func (b *Billing) Price(ctx Context, id string) (*Price, error) {
	if b.gateway == nil {
		return nil, ErrNoGateway
	}
	if id == "" {
		return nil, errors.New("pay: Price needs an id")
	}
	return b.gateway.Price(ctx, id)
}
