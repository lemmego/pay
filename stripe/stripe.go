// Package stripe implements pay.Gateway against Stripe's REST API.
//
// Over net/http and encoding/json rather than Stripe's own SDK. Four reasons,
// in order of how much they matter:
//
//   - The surface this product uses is three endpoints. The SDK is a few
//     hundred thousand lines of generated types that would be compiled into
//     every binary, including the CLI.
//   - The SDK pins an API version in its code. Upgrading it is a
//     breaking-change review of every field it touches, which is a strange
//     thing to schedule in order to get a bug fix elsewhere.
//   - Its types model everything, so a caller handles cases this product
//     cannot produce. The flattening has to happen somewhere; doing it at the
//     boundary means it happens once.
//   - A hand-rolled client is testable against an httptest server, so every
//     request this package makes — form encoding, idempotency key, account
//     header — is asserted rather than assumed.
//
// The cost is honest: new Stripe features need code here, and a response
// field this file does not know about is ignored. Both are fine for three
// endpoints, and neither is fine for a package that grows to thirty.
package stripe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lemmego/pay"
)

// APIBase is Stripe's API root.
const APIBase = "https://api.stripe.com"

// APIVersion pins the response shape.
//
// Pinned rather than left to the account's default, because the default is a
// dashboard setting: somebody clicking "upgrade API version" in Stripe would
// otherwise change what this code receives, with no deploy and no review. A
// version is sent on every request so the shape is a property of this code.
const APIVersion = "2024-06-20"

// Config configures the gateway.
type Config struct {
	// SecretKey is the platform's own secret key.
	SecretKey string
	// BaseURL overrides the API root. Tests point it at an httptest server;
	// nothing else should set it.
	BaseURL string
	// HTTPClient overrides the client. The default has a timeout, which the
	// http package's default does not — an unbounded payment call holds a
	// request goroutine until the far end feels like answering.
	HTTPClient *http.Client
	// Secrets are the webhook signing secrets, so Verify can authenticate a
	// delivery. Billing checks the signature too; this is here because
	// pay.Gateway.Verify is defined to authenticate, and a gateway that
	// parsed without verifying would be a loaded gun for any other caller.
	Secrets []string
	// Tolerance overrides the signature timestamp window.
	Tolerance time.Duration
}

// Gateway is the Stripe implementation of pay.Gateway.
type Gateway struct {
	cfg    Config
	client *http.Client
	base   string
}

// New returns a Stripe gateway.
func New(cfg Config) *Gateway {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	base := strings.TrimSuffix(cfg.BaseURL, "/")
	if base == "" {
		base = APIBase
	}
	return &Gateway{cfg: cfg, client: client, base: base}
}

// Name identifies rows this gateway wrote.
func (g *Gateway) Name() string { return "stripe" }

// Livemode reports whether the configured key is a live one.
//
// Read off the key's prefix rather than configured separately. A key and a
// mode flag that can disagree eventually do, and the failure is silent in the
// expensive direction: a test-mode event, which anybody can cause, applied to
// a production row.
func (g *Gateway) Livemode() bool {
	return strings.HasPrefix(g.cfg.SecretKey, "sk_live_") ||
		strings.HasPrefix(g.cfg.SecretKey, "rk_live_")
}

// Checkout creates a hosted subscription checkout session.
func (g *Gateway) Checkout(ctx context.Context, req pay.CheckoutRequest) (*pay.Checkout, error) {
	form := url.Values{}
	form.Set("mode", "subscription")
	form.Set("line_items[0][price]", req.PriceID)
	form.Set("line_items[0][quantity]", "1")
	form.Set("client_reference_id", req.ClientReference)
	form.Set("success_url", req.SuccessURL)
	form.Set("cancel_url", req.CancelURL)
	// Without this the portal cannot be opened later: a session that creates
	// its own customer reports one, but only if we ask for the customer to be
	// created rather than guessed at.
	if req.CustomerID != "" {
		form.Set("customer", req.CustomerID)
	} else {
		form.Set("customer_creation", "always")
		if req.Email != "" {
			form.Set("customer_email", req.Email)
		}
	}
	if req.TrialDays > 0 {
		form.Set("subscription_data[trial_period_days]", strconv.Itoa(req.TrialDays))
	}

	var out struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := g.post(ctx, "/v1/checkout/sessions", form, req.IdempotencyKey, "", &out); err != nil {
		return nil, err
	}
	if out.URL == "" {
		return nil, errors.New("stripe: the checkout session has no URL")
	}
	return &pay.Checkout{ID: out.ID, URL: out.URL}, nil
}

// Portal creates a billing portal session.
func (g *Gateway) Portal(ctx context.Context, customerID, returnURL string) (string, error) {
	form := url.Values{}
	form.Set("customer", customerID)
	form.Set("return_url", returnURL)

	var out struct {
		URL string `json:"url"`
	}
	// No idempotency key: a portal session is a short-lived read-only link
	// that charges nothing, so a retry creating a second one costs nothing.
	// An idempotency key would actively hurt — the replayed response holds a
	// link that may already have expired.
	if err := g.post(ctx, "/v1/billing_portal/sessions", form, "", "", &out); err != nil {
		return "", err
	}
	if out.URL == "" {
		return "", errors.New("stripe: the portal session has no URL")
	}
	return out.URL, nil
}

// Subscription reads one subscription.
func (g *Gateway) Subscription(ctx context.Context, id string) (*pay.Subscription, error) {
	var raw subscription
	if err := g.get(ctx, "/v1/subscriptions/"+url.PathEscape(id), nil, "", &raw); err != nil {
		return nil, err
	}
	return raw.convert(), nil
}

// Price reads a price.
func (g *Gateway) Price(ctx context.Context, id string) (*pay.Price, error) {
	if id == "" {
		return nil, errors.New("stripe: no price id")
	}
	var raw struct {
		ID         string `json:"id"`
		UnitAmount int64  `json:"unit_amount"`
		Currency   string `json:"currency"`
		Recurring  struct {
			Interval      string `json:"interval"`
			IntervalCount int64  `json:"interval_count"`
		} `json:"recurring"`
	}
	if err := g.get(ctx, "/v1/prices/"+url.PathEscape(id), nil, "", &raw); err != nil {
		return nil, err
	}
	return &pay.Price{
		ID:            raw.ID,
		Amount:        pay.Money{Amount: raw.UnitAmount, Currency: raw.Currency},
		Interval:      pay.Interval(raw.Recurring.Interval),
		IntervalCount: raw.Recurring.IntervalCount,
	}, nil
}

// Verify authenticates a delivery and parses the event it carries.
func (g *Gateway) Verify(payload []byte, signature string, now time.Time) (*pay.Event, error) {
	if err := pay.VerifySignature(payload, signature, g.cfg.Secrets, now, g.cfg.Tolerance); err != nil {
		return nil, err
	}
	return ParseEvent(payload)
}

// ParseEvent decodes a verified payload into an event.
//
// Exported, and separate from Verify, so a test can build an event without a
// signature. It is named for what it does — it does not authenticate
// anything, and a caller reaching for it on an unverified payload is doing
// something visibly wrong.
func ParseEvent(payload []byte) (*pay.Event, error) {
	var envelope struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Created  int64  `json:"created"`
		Livemode bool   `json:"livemode"`
		Data     struct {
			Object json.RawMessage `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("stripe: the event payload is not JSON: %w", err)
	}
	if envelope.ID == "" || envelope.Type == "" {
		return nil, errors.New("stripe: the event payload has no id or type")
	}

	event := &pay.Event{
		ID:        envelope.ID,
		Type:      envelope.Type,
		CreatedAt: time.Unix(envelope.Created, 0).UTC(),
		Livemode:  envelope.Livemode,
		Data:      envelope.Data.Object,
	}

	switch {
	case strings.HasPrefix(envelope.Type, "customer.subscription."):
		var raw subscription
		if err := json.Unmarshal(envelope.Data.Object, &raw); err != nil {
			return nil, fmt.Errorf("stripe: %s carries no subscription: %w", envelope.Type, err)
		}
		event.Subscription = raw.convert()
		event.CustomerID = event.Subscription.CustomerID

	case envelope.Type == "checkout.session.completed":
		var raw struct {
			Customer          any    `json:"customer"`
			ClientReferenceID string `json:"client_reference_id"`
			Subscription      any    `json:"subscription"`
			Mode              string `json:"mode"`
		}
		if err := json.Unmarshal(envelope.Data.Object, &raw); err != nil {
			return nil, fmt.Errorf("stripe: %s is malformed: %w", envelope.Type, err)
		}
		event.CustomerID = expandedID(raw.Customer)
		event.ClientReference = raw.ClientReferenceID
		// The session carries the subscription's id and not its state, so
		// this event attributes the purchase to a person and nothing more.
		// The subscription's own events carry the state, and they are the
		// only thing that grants entitlement — which is why a completed
		// checkout alone is not enough to turn Pro on.
		if id := expandedID(raw.Subscription); id != "" {
			event.Subscription = &pay.Subscription{ID: id, CustomerID: event.CustomerID}
		}

	default:
		// Everything else is returned with no object. The handler answers 200
		// and does nothing, which is what stops Stripe retrying an event this
		// application has no opinion about.
	}

	return event, nil
}

// expandedID reads an id that Stripe sends either as a string or, when the
// field was expanded, as an object with an id.
//
// Both shapes appear for the same field depending on how the event was
// configured, and a decoder that assumes the string form fails at runtime on
// an account whose webhook expands it — which is a setting in somebody else's
// dashboard.
func expandedID(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case map[string]any:
		if id, ok := v["id"].(string); ok {
			return id
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Wire shapes
//
// Only the fields this product reads. A response field that is not here is
// ignored, which is the trade this package makes on purpose.

type subscription struct {
	ID       string `json:"id"`
	Customer any    `json:"customer"`
	Status   string `json:"status"`

	CurrentPeriodEnd  int64 `json:"current_period_end"`
	CancelAtPeriodEnd bool  `json:"cancel_at_period_end"`
	CanceledAt        int64 `json:"canceled_at"`
	TrialEnd          int64 `json:"trial_end"`

	Items struct {
		Data []struct {
			Quantity int64 `json:"quantity"`
			Price    struct {
				ID         string `json:"id"`
				UnitAmount int64  `json:"unit_amount"`
				Currency   string `json:"currency"`
				Recurring  struct {
					Interval      string `json:"interval"`
					IntervalCount int64  `json:"interval_count"`
				} `json:"recurring"`
			} `json:"price"`
		} `json:"data"`
		HasMore bool `json:"has_more"`
	} `json:"items"`

	Discount *struct {
		Coupon struct {
			PercentOff float64 `json:"percent_off"`
			AmountOff  int64   `json:"amount_off"`
			Currency   string  `json:"currency"`
		} `json:"coupon"`
	} `json:"discount"`
}

func (s *subscription) convert() *pay.Subscription {
	out := &pay.Subscription{
		ID:                s.ID,
		CustomerID:        expandedID(s.Customer),
		Status:            pay.Status(s.Status),
		CancelAtPeriodEnd: s.CancelAtPeriodEnd,
		CurrentPeriodEnd:  unix(s.CurrentPeriodEnd),
		CanceledAt:        unix(s.CanceledAt),
		TrialEndsAt:       unix(s.TrialEnd),
		Quantity:          1,
	}

	if len(s.Items.Data) > 0 {
		item := s.Items.Data[0]
		out.PriceID = item.Price.ID
		out.UnitAmount = pay.Money{Amount: item.Price.UnitAmount, Currency: item.Price.Currency}
		out.Interval = pay.Interval(item.Price.Recurring.Interval)
		out.IntervalCount = item.Price.Recurring.IntervalCount
		if item.Quantity > 0 {
			out.Quantity = item.Quantity
		}
	}
	// has_more as well as a length check: a subscription with eleven items
	// arrives with ten and has_more set, and reading only the length would
	// call that a single-line subscription.
	out.MultipleItems = len(s.Items.Data) > 1 || s.Items.HasMore

	if s.Discount != nil {
		out.DiscountPercent = s.Discount.Coupon.PercentOff
		if s.Discount.Coupon.AmountOff > 0 {
			out.DiscountAmount = pay.Money{
				Amount:   s.Discount.Coupon.AmountOff,
				Currency: s.Discount.Coupon.Currency,
			}
		}
	}
	return out
}

func unix(seconds int64) *time.Time {
	if seconds <= 0 {
		return nil
	}
	t := time.Unix(seconds, 0).UTC()
	return &t
}

// ---------------------------------------------------------------------------
// Transport

type apiError struct {
	Status  int
	Type    string
	Code    string
	Message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("stripe: %d %s/%s: %s", e.Status, e.Type, e.Code, e.Message)
}

// Retryable reports whether the call is worth making again.
//
// A 409 is Stripe's lock contention and a 429 is its rate limiter; both pass
// on a second attempt. A 4xx otherwise is a request this code got wrong and
// will keep getting wrong. 5xx is theirs and may not recur.
func (e *apiError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests ||
		e.Status == http.StatusConflict ||
		e.Status >= 500
}

func (g *Gateway) post(ctx context.Context, path string, form url.Values, idempotencyKey, account string, out any) error {
	return g.do(ctx, http.MethodPost, path, form, idempotencyKey, account, out)
}

func (g *Gateway) get(ctx context.Context, path string, query url.Values, account string, out any) error {
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return g.do(ctx, http.MethodGet, path, nil, "", account, out)
}

func (g *Gateway) do(ctx context.Context, method, path string, form url.Values, idempotencyKey, account string, out any) error {
	if g.cfg.SecretKey == "" {
		return pay.ErrNoGateway
	}

	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	request, err := http.NewRequestWithContext(ctx, method, g.base+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+g.cfg.SecretKey)
	request.Header.Set("Stripe-Version", APIVersion)
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	// Stripe-Account is how one request acts on a connected account. It is
	// the only difference between reading our own books and reading
	// somebody's who authorised us, so it is set explicitly per call and
	// never held on the client.
	if account != "" {
		request.Header.Set("Stripe-Account", account)
	}

	response, err := g.client.Do(request)
	if err != nil {
		return fmt.Errorf("stripe: %s %s: %w", method, path, err)
	}
	defer response.Body.Close()

	// Bounded: this is somebody else's server and an unbounded read of an
	// error page is an unbounded allocation.
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("stripe: reading the response to %s %s: %w", method, path, err)
	}

	if response.StatusCode >= 300 {
		var wrapper struct {
			Error struct {
				Type    string `json:"type"`
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(payload, &wrapper)
		return &apiError{
			Status:  response.StatusCode,
			Type:    wrapper.Error.Type,
			Code:    wrapper.Error.Code,
			Message: wrapper.Error.Message,
		}
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("stripe: decoding the response to %s %s: %w", method, path, err)
	}
	return nil
}
