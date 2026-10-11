// Package paytest is a pay.Gateway that charges nobody.
//
// Every test in an application that sells something needs one of these, and
// the alternative is each project writing its own — which means each project
// decides for itself what a webhook looks like, and the shapes drift until a
// test proves nothing about the real thing.
package paytest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/lemmego/pay"
)

// Gateway records what it was asked to do and returns what it was told to.
type Gateway struct {
	mu sync.Mutex

	// Secret signs deliveries built by Sign.
	Secret string
	// Live is what the events this gateway signs claim to be.
	Live bool
	// CheckoutURL is what Checkout returns.
	CheckoutURL string
	// PortalURL is what Portal returns.
	PortalURL string
	// Err, when set, is returned by every call.
	Err error

	// Subscriptions is what Subscription looks up, keyed by id.
	Subscriptions map[string]*pay.Subscription
	// Prices is what Price looks up, keyed by id.
	Prices map[string]*pay.Price

	checkouts []pay.CheckoutRequest
	portals   []string
}

// New returns a gateway with a signing secret.
func New(secret string) *Gateway {
	return &Gateway{
		Secret:        secret,
		CheckoutURL:   "https://checkout.test/session",
		PortalURL:     "https://portal.test/session",
		Subscriptions: map[string]*pay.Subscription{},
		Prices: map[string]*pay.Price{
			"price_pro": {
				ID:            "price_pro",
				Amount:        pay.Money{Amount: 900, Currency: "usd"},
				Interval:      pay.Monthly,
				IntervalCount: 1,
			},
		},
	}
}

func (g *Gateway) Name() string   { return "stripe" }
func (g *Gateway) Livemode() bool { return g.Live }

// Checkout records the request and returns the configured URL.
func (g *Gateway) Checkout(ctx pay.Context, req pay.CheckoutRequest) (*pay.Checkout, error) {
	if g.Err != nil {
		return nil, g.Err
	}
	g.mu.Lock()
	g.checkouts = append(g.checkouts, req)
	g.mu.Unlock()
	return &pay.Checkout{ID: "cs_test_" + strconv.Itoa(len(g.checkouts)), URL: g.CheckoutURL}, nil
}

// Portal records the customer and returns the configured URL.
func (g *Gateway) Portal(ctx pay.Context, customerID, returnURL string) (string, error) {
	if g.Err != nil {
		return "", g.Err
	}
	g.mu.Lock()
	g.portals = append(g.portals, customerID)
	g.mu.Unlock()
	return g.PortalURL, nil
}

// Subscription returns a configured subscription.
func (g *Gateway) Subscription(ctx pay.Context, id string) (*pay.Subscription, error) {
	if g.Err != nil {
		return nil, g.Err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if s, ok := g.Subscriptions[id]; ok {
		return s, nil
	}
	return nil, fmt.Errorf("paytest: no subscription %q", id)
}

// Price returns a configured price.
func (g *Gateway) Price(ctx pay.Context, id string) (*pay.Price, error) {
	if g.Err != nil {
		return nil, g.Err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if p, ok := g.Prices[id]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("paytest: no price %q", id)
}

// Verify authenticates with the same code the real gateway uses.
//
// The same code deliberately: a fake that skips verification lets a test pass
// against a handler that never checks a signature, which is the one property
// of a webhook endpoint worth testing.
func (g *Gateway) Verify(payload []byte, signature string, now time.Time) (*pay.Event, error) {
	if err := pay.VerifySignature(payload, signature, []string{g.Secret}, now, 0); err != nil {
		return nil, err
	}
	return ParseEvent(payload)
}

// Checkouts returns the checkout requests this gateway was given.
func (g *Gateway) Checkouts() []pay.CheckoutRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]pay.CheckoutRequest(nil), g.checkouts...)
}

// Portals returns the customers a portal was opened for.
func (g *Gateway) Portals() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.portals...)
}

// ParseEvent decodes an event built by Event.
func ParseEvent(payload []byte) (*pay.Event, error) {
	var wire struct {
		ID           string            `json:"id"`
		Type         string            `json:"type"`
		Created      int64             `json:"created"`
		Livemode     bool              `json:"livemode"`
		Subscription *pay.Subscription `json:"subscription"`
		Customer     string            `json:"customer"`
		Reference    string            `json:"client_reference_id"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		return nil, err
	}
	if wire.ID == "" || wire.Type == "" {
		return nil, fmt.Errorf("paytest: the payload has no id or type")
	}
	return &pay.Event{
		ID:              wire.ID,
		Type:            wire.Type,
		CreatedAt:       time.Unix(wire.Created, 0).UTC(),
		Livemode:        wire.Livemode,
		Subscription:    wire.Subscription,
		CustomerID:      wire.Customer,
		ClientReference: wire.Reference,
		Data:            payload,
	}, nil
}

// Event builds a delivery payload.
func (g *Gateway) Event(id, kind string, at time.Time, s *pay.Subscription, reference string) []byte {
	customer := ""
	if s != nil {
		customer = s.CustomerID
	}
	payload, _ := json.Marshal(map[string]any{
		"id":                  id,
		"type":                kind,
		"created":             at.Unix(),
		"livemode":            g.Live,
		"subscription":        s,
		"customer":            customer,
		"client_reference_id": reference,
	})
	return payload
}

// Sign returns the signature header for a payload, as the gateway would send
// it.
func (g *Gateway) Sign(payload []byte, at time.Time) string {
	timestamp := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(g.Secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(payload)
	return "t=" + timestamp + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// ---------------------------------------------------------------------------

// Reader is a pay.RevenueReader and pay.Authorizer over a fixed list.
type Reader struct {
	// Accounts maps a connected account to its subscriptions.
	Accounts map[string][]*pay.Subscription
	// Err, when set, is returned instead of listing.
	Err error
	// Revoked accounts fail Account, as a real one does once access is
	// withdrawn.
	Revoked map[string]bool
	// Codes maps an authorisation code to the account it grants. An unknown
	// code is refused, the way a used or forged one is.
	Codes map[string]string
	// AuthorizeHost is where AuthorizeURL points.
	AuthorizeHost string
}

// NewReader returns an empty reader.
func NewReader() *Reader {
	return &Reader{
		Accounts:      map[string][]*pay.Subscription{},
		Revoked:       map[string]bool{},
		Codes:         map[string]string{},
		AuthorizeHost: "https://connect.test",
	}
}

// AuthorizeURL is where the account owner would be sent.
//
// It refuses an empty state for the same reason the real one does, so a
// handler that forgot to bind one fails in a test rather than in production.
func (r *Reader) AuthorizeURL(state, redirectURI string) (string, error) {
	if state == "" {
		return "", fmt.Errorf("paytest: AuthorizeURL needs a state bound to the session")
	}
	return r.AuthorizeHost + "/oauth/authorize?scope=read_only&state=" +
		url.QueryEscape(state) + "&redirect_uri=" + url.QueryEscape(redirectURI), nil
}

// Exchange turns a configured code into its account.
func (r *Reader) Exchange(ctx pay.Context, code string) (string, error) {
	if account, ok := r.Codes[code]; ok {
		return account, nil
	}
	return "", fmt.Errorf("paytest: no authorisation code %q", code)
}

// Subscriptions walks the account's list.
func (r *Reader) Subscriptions(ctx pay.Context, account string, each func(*pay.Subscription) error) error {
	if r.Err != nil {
		return r.Err
	}
	for _, s := range r.Accounts[account] {
		if err := each(s); err != nil {
			return err
		}
	}
	return nil
}

// Account reports whether the authorisation still stands.
func (r *Reader) Account(ctx pay.Context, account string) error {
	if r.Revoked[account] {
		return fmt.Errorf("paytest: access to %s has been revoked", account)
	}
	if _, ok := r.Accounts[account]; !ok {
		return fmt.Errorf("paytest: no account %q", account)
	}
	return nil
}
