package stripe_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lemmego/pay"
	"github.com/lemmego/pay/stripe"
)

// recorder is Stripe, for the purposes of asserting what we send it.
type recorder struct {
	requests []*http.Request
	forms    []url.Values
	respond  func(path string) (int, string)
}

func serve(t *testing.T, respond func(path string) (int, string)) (*stripe.Gateway, *recorder) {
	t.Helper()
	rec := &recorder{respond: respond}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		rec.requests = append(rec.requests, r)
		rec.forms = append(rec.forms, r.PostForm)
		status, body := rec.respond(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	return stripe.New(stripe.Config{
		SecretKey: "sk_test_abc",
		BaseURL:   server.URL,
		Secrets:   []string{"whsec_test"},
	}), rec
}

func TestCheckoutSendsWhatStripeNeeds(t *testing.T) {
	gateway, rec := serve(t, func(string) (int, string) {
		return 200, `{"id":"cs_test_1","url":"https://checkout.stripe.com/c/pay/cs_test_1"}`
	})

	checkout, err := gateway.Checkout(context.Background(), pay.CheckoutRequest{
		PriceID:         "price_pro",
		Email:           "ada@example.com",
		ClientReference: "42",
		SuccessURL:      "https://productshore.test/settings/billing?done=1",
		CancelURL:       "https://productshore.test/pro",
		IdempotencyKey:  "user-42-pro-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if checkout.URL != "https://checkout.stripe.com/c/pay/cs_test_1" {
		t.Errorf("url = %q", checkout.URL)
	}

	form := rec.forms[0]
	for key, want := range map[string]string{
		"mode":                 "subscription",
		"line_items[0][price]": "price_pro",
		"client_reference_id":  "42",
		"customer_creation":    "always",
		"customer_email":       "ada@example.com",
	} {
		if got := form.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	request := rec.requests[0]
	// The idempotency key is why a double-submitted form bills once.
	if got := request.Header.Get("Idempotency-Key"); got != "user-42-pro-v1" {
		t.Errorf("Idempotency-Key = %q", got)
	}
	// The API version is pinned here rather than left to a dashboard setting
	// somebody could change without a deploy.
	if got := request.Header.Get("Stripe-Version"); got != stripe.APIVersion {
		t.Errorf("Stripe-Version = %q, want %q", got, stripe.APIVersion)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer sk_test_abc" {
		t.Errorf("Authorization = %q", got)
	}
	// Not a connected-account call: reading our own books must not carry a
	// Stripe-Account header picked up from somewhere.
	if got := request.Header.Get("Stripe-Account"); got != "" {
		t.Errorf("Stripe-Account = %q on a platform call", got)
	}
}

func TestCheckoutReusesAnExistingCustomer(t *testing.T) {
	gateway, rec := serve(t, func(string) (int, string) {
		return 200, `{"id":"cs_1","url":"https://checkout.test/x"}`
	})
	if _, err := gateway.Checkout(context.Background(), pay.CheckoutRequest{
		PriceID: "price_pro", ClientReference: "42", CustomerID: "cus_9",
	}); err != nil {
		t.Fatal(err)
	}
	form := rec.forms[0]
	if got := form.Get("customer"); got != "cus_9" {
		t.Errorf("customer = %q", got)
	}
	// One person, one customer record: asking Stripe to create another would
	// split their billing history in two.
	if got := form.Get("customer_creation"); got != "" {
		t.Errorf("customer_creation = %q with a customer already known", got)
	}
}

func TestLivemodeComesOffTheKey(t *testing.T) {
	// Derived rather than configured, so a flag cannot disagree with the key
	// actually in use.
	for key, live := range map[string]bool{
		"sk_test_abc": false,
		"sk_live_abc": true,
		"rk_live_abc": true,
		"":            false,
	} {
		if got := stripe.New(stripe.Config{SecretKey: key}).Livemode(); got != live {
			t.Errorf("%q: Livemode = %v, want %v", key, got, live)
		}
	}
}

func TestSubscriptionConversion(t *testing.T) {
	// The customer arrives as an object, which is what an expanded webhook
	// sends — a decoder that assumes a string fails on somebody else's
	// dashboard setting.
	body := `{
      "id": "sub_1",
      "customer": {"id": "cus_1", "email": "ada@example.com"},
      "status": "active",
      "current_period_end": 1800000000,
      "cancel_at_period_end": true,
      "items": {"data": [{"quantity": 3, "price": {
          "id": "price_pro", "unit_amount": 1900, "currency": "usd",
          "recurring": {"interval": "month", "interval_count": 1}}}],
        "has_more": false},
      "discount": {"coupon": {"percent_off": 20}}
    }`
	gateway, _ := serve(t, func(string) (int, string) { return 200, body })

	sub, err := gateway.Subscription(context.Background(), "sub_1")
	if err != nil {
		t.Fatal(err)
	}
	if sub.CustomerID != "cus_1" {
		t.Errorf("customer = %q, want cus_1 from the expanded object", sub.CustomerID)
	}
	if sub.Status != pay.StatusActive || sub.Quantity != 3 || sub.PriceID != "price_pro" {
		t.Errorf("converted to %+v", sub)
	}
	if sub.UnitAmount != (pay.Money{Amount: 1900, Currency: "usd"}) {
		t.Errorf("unit amount = %s", sub.UnitAmount)
	}
	if !sub.CancelAtPeriodEnd {
		t.Error("cancel_at_period_end was dropped")
	}
	if sub.CurrentPeriodEnd == nil || sub.CurrentPeriodEnd.Unix() != 1800000000 {
		t.Errorf("period end = %v", sub.CurrentPeriodEnd)
	}
	// 3 × $19.00 less 20%.
	if got := sub.MonthlyRevenue(); got.Amount != 4560 {
		t.Errorf("monthly revenue = %s, want 4560", got)
	}
}

// TestHasMoreMakesASubscriptionMultiLine is the eleven-item case: Stripe
// sends ten and sets has_more, so a length check alone calls it single-line.
func TestHasMoreMakesASubscriptionMultiLine(t *testing.T) {
	body := `{"id":"sub_1","customer":"cus_1","status":"active",
      "items":{"data":[{"quantity":1,"price":{"id":"p","unit_amount":100,"currency":"usd",
      "recurring":{"interval":"month","interval_count":1}}}],"has_more":true}}`
	gateway, _ := serve(t, func(string) (int, string) { return 200, body })

	sub, err := gateway.Subscription(context.Background(), "sub_1")
	if err != nil {
		t.Fatal(err)
	}
	if !sub.MultipleItems {
		t.Error("has_more did not mark the subscription multi-line")
	}
}

func TestAnAPIErrorIsReportedWithItsStatus(t *testing.T) {
	gateway, _ := serve(t, func(string) (int, string) {
		return 402, `{"error":{"type":"card_error","code":"card_declined","message":"Your card was declined."}}`
	})
	_, err := gateway.Checkout(context.Background(), pay.CheckoutRequest{PriceID: "p", ClientReference: "1"})
	if err == nil {
		t.Fatal("a 402 was reported as success")
	}
	if !strings.Contains(err.Error(), "card_declined") {
		t.Errorf("error = %v", err)
	}
}

func TestWithNoKeyNothingIsSent(t *testing.T) {
	gateway := stripe.New(stripe.Config{})
	// ErrNoGateway rather than an unauthenticated request to Stripe: a
	// development boot with no credentials should refuse locally, not go out
	// over the network to be refused.
	if _, err := gateway.Checkout(context.Background(), pay.CheckoutRequest{PriceID: "p", ClientReference: "1"}); !errors.Is(err, pay.ErrNoGateway) {
		t.Fatalf("got %v, want ErrNoGateway", err)
	}
}

func TestPortalSendsNoIdempotencyKey(t *testing.T) {
	gateway, rec := serve(t, func(string) (int, string) {
		return 200, `{"url":"https://billing.stripe.com/session/x"}`
	})
	url, err := gateway.Portal(context.Background(), "cus_1", "https://productshore.test/settings/billing")
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://billing.stripe.com/session/x" {
		t.Errorf("url = %q", url)
	}
	// A replayed idempotent response holds a link that may already have
	// expired, and the call charges nothing, so there is nothing to protect.
	if got := rec.requests[0].Header.Get("Idempotency-Key"); got != "" {
		t.Errorf("Idempotency-Key = %q on a portal session", got)
	}
}

func TestParseEventAttributesACompletedCheckout(t *testing.T) {
	payload := []byte(`{"id":"evt_1","type":"checkout.session.completed","created":1700000000,
      "livemode":false,"data":{"object":{"mode":"subscription","customer":"cus_1",
      "client_reference_id":"42","subscription":"sub_1"}}}`)

	event, err := stripe.ParseEvent(payload)
	if err != nil {
		t.Fatal(err)
	}
	if event.ClientReference != "42" || event.CustomerID != "cus_1" {
		t.Errorf("attribution = %q / %q", event.ClientReference, event.CustomerID)
	}
	// The session carries the subscription's id and not its state, so this
	// event attributes a purchase and grants nothing.
	if event.Subscription == nil || event.Subscription.ID != "sub_1" {
		t.Fatalf("subscription = %+v", event.Subscription)
	}
	if event.Subscription.Status != "" {
		t.Errorf("a completed checkout reported a status (%q), which would be enough to grant Pro",
			event.Subscription.Status)
	}
	if !event.CreatedAt.Equal(time.Unix(1700000000, 0).UTC()) {
		t.Errorf("created = %v", event.CreatedAt)
	}
}

func TestParseEventLeavesAnUnknownTypeAlone(t *testing.T) {
	payload := []byte(`{"id":"evt_1","type":"radar.early_fraud_warning.created",
      "created":1700000000,"data":{"object":{"id":"issfr_1"}}}`)
	event, err := stripe.ParseEvent(payload)
	if err != nil {
		t.Fatal(err)
	}
	// No object and no error: the handler answers 200 and does nothing, which
	// is what stops Stripe retrying an event we have no opinion about.
	if event.Subscription != nil {
		t.Error("an unrelated event produced a subscription")
	}
	if event.Type != "radar.early_fraud_warning.created" {
		t.Errorf("type = %q", event.Type)
	}
}

func TestParseEventRefusesRubbish(t *testing.T) {
	for name, payload := range map[string]string{
		"not json": `{`,
		"no id":    `{"type":"customer.subscription.updated"}`,
		"no type":  `{"id":"evt_1"}`,
	} {
		if _, err := stripe.ParseEvent([]byte(payload)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestVerifyWillNotParseAnUnsignedPayload(t *testing.T) {
	gateway := stripe.New(stripe.Config{SecretKey: "sk_test_x", Secrets: []string{"whsec_test"}})
	payload := []byte(`{"id":"evt_1","type":"customer.subscription.updated","created":1}`)
	if _, err := gateway.Verify(payload, "t=1,v1=00", time.Now()); !errors.Is(err, pay.ErrBadSignature) {
		t.Fatalf("got %v, want ErrBadSignature", err)
	}
}
