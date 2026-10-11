package pay_test

import (
	"errors"
	"testing"
	"time"

	"github.com/lemmego/pay"
	"github.com/lemmego/pay/paytest"
)

// store is a Store that keeps what it was given, including the ordering guard
// a real implementation puts in its WHERE clause.
type store struct {
	seen      map[string]bool
	applied   []*pay.Event
	appliedAt time.Time
	failSeen  error
	failApply error
}

func newStore() *store { return &store{seen: map[string]bool{}} }

func (s *store) Seen(ctx pay.Context, event *pay.Event) (bool, error) {
	if s.failSeen != nil {
		return false, s.failSeen
	}
	if s.seen[event.ID] {
		return false, nil
	}
	s.seen[event.ID] = true
	return true, nil
}

func (s *store) Apply(ctx pay.Context, event *pay.Event) error {
	if s.failApply != nil {
		return s.failApply
	}
	// The guard a real store expresses as a condition on the write: an event
	// older than what the row was last written by loses.
	if !s.appliedAt.IsZero() && event.CreatedAt.Before(s.appliedAt) {
		return nil
	}
	s.appliedAt = event.CreatedAt
	s.applied = append(s.applied, event)
	return nil
}

func activeSub() *pay.Subscription {
	end := time.Now().Add(20 * 24 * time.Hour)
	return &pay.Subscription{
		ID: "sub_1", CustomerID: "cus_1", Status: pay.StatusActive,
		UnitAmount: usd(900), Interval: pay.Monthly, IntervalCount: 1, Quantity: 1,
		CurrentPeriodEnd: &end,
	}
}

func TestHandleAppliesAVerifiedDelivery(t *testing.T) {
	gateway := paytest.New("whsec_test")
	st := newStore()
	billing := pay.New(gateway, st, pay.WithSecrets("whsec_test"))

	now := time.Now()
	payload := gateway.Event("evt_1", "customer.subscription.updated", now, activeSub(), "7")

	if err := billing.Handle(nil, payload, gateway.Sign(payload, now)); err != nil {
		t.Fatal(err)
	}
	if len(st.applied) != 1 {
		t.Fatalf("applied %d events, want 1", len(st.applied))
	}
	if st.applied[0].Subscription.ID != "sub_1" {
		t.Errorf("applied %+v", st.applied[0].Subscription)
	}
}

// TestARetriedDeliveryIsAppliedOnce is the behaviour Stripe's retry schedule
// makes mandatory. It retries until it gets a 2xx, and it retries again after
// a success it did not hear about.
func TestARetriedDeliveryIsAppliedOnce(t *testing.T) {
	gateway := paytest.New("whsec_test")
	st := newStore()
	billing := pay.New(gateway, st, pay.WithSecrets("whsec_test"))

	now := time.Now()
	payload := gateway.Event("evt_1", "customer.subscription.updated", now, activeSub(), "7")
	signature := gateway.Sign(payload, now)

	for i := 0; i < 3; i++ {
		if err := billing.Handle(nil, payload, signature); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	if len(st.applied) != 1 {
		t.Fatalf("applied %d times, want 1", len(st.applied))
	}
}

// TestAnOlderEventDoesNotOverwriteANewerOne is the out-of-order case. Stripe
// promises no ordering, so a cancellation retried after a renewal must not
// put the row back.
func TestAnOlderEventDoesNotOverwriteANewerOne(t *testing.T) {
	gateway := paytest.New("whsec_test")
	st := newStore()
	billing := pay.New(gateway, st, pay.WithSecrets("whsec_test"))

	now := time.Now()
	newer := gateway.Event("evt_2", "customer.subscription.updated", now, activeSub(), "7")

	canceled := activeSub()
	canceled.Status = pay.StatusCanceled
	older := gateway.Event("evt_1", "customer.subscription.updated", now.Add(-time.Hour), canceled, "7")

	if err := billing.Handle(nil, newer, gateway.Sign(newer, now)); err != nil {
		t.Fatal(err)
	}
	if err := billing.Handle(nil, older, gateway.Sign(older, now)); err != nil {
		t.Fatal(err)
	}

	if len(st.applied) != 1 {
		t.Fatalf("applied %d events, want 1 — the older one should have lost", len(st.applied))
	}
	if st.applied[0].ID != "evt_2" {
		t.Errorf("the surviving event is %s, want evt_2", st.applied[0].ID)
	}
}

func TestHandleRefusesAForgedDelivery(t *testing.T) {
	gateway := paytest.New("whsec_test")
	st := newStore()
	billing := pay.New(gateway, st, pay.WithSecrets("whsec_test"))

	now := time.Now()
	payload := gateway.Event("evt_1", "customer.subscription.updated", now, activeSub(), "7")

	// Anybody can POST this JSON. Only the signature stops it being a free
	// subscription, so this is the single most important refusal here.
	if err := billing.Handle(nil, payload, "t=1,v1=00"); !errors.Is(err, pay.ErrBadSignature) {
		t.Fatalf("got %v, want ErrBadSignature", err)
	}
	if len(st.applied) != 0 {
		t.Fatal("a forged delivery was applied")
	}
	if len(st.seen) != 0 {
		t.Fatal("a forged delivery was recorded as seen, which would make the real one a duplicate")
	}
}

// TestATestModeEventIsRefusedInLiveMode closes the hole that a signature
// check alone leaves open: a test-mode event is genuinely signed by Stripe,
// and anybody who can reach the dashboard or the CLI can cause one.
func TestATestModeEventIsRefusedInLiveMode(t *testing.T) {
	gateway := paytest.New("whsec_test")
	gateway.Live = false
	st := newStore()
	billing := pay.New(gateway, st, pay.WithSecrets("whsec_test"), pay.WithLivemode(true))

	now := time.Now()
	payload := gateway.Event("evt_1", "customer.subscription.updated", now, activeSub(), "7")

	if err := billing.Handle(nil, payload, gateway.Sign(payload, now)); !errors.Is(err, pay.ErrBadSignature) {
		t.Fatalf("got %v, want a refusal", err)
	}
	if len(st.applied) != 0 {
		t.Fatal("a test-mode event was applied to a live endpoint")
	}
}

func TestHandleWithNoStoreFailsRatherThanDiscarding(t *testing.T) {
	gateway := paytest.New("whsec_test")
	billing := pay.New(gateway, nil, pay.WithSecrets("whsec_test"))

	now := time.Now()
	payload := gateway.Event("evt_1", "customer.subscription.updated", now, activeSub(), "7")

	// A 500 and a retry, not a 200 and a shrug: the event was real and
	// nothing recorded it.
	if err := billing.Handle(nil, payload, gateway.Sign(payload, now)); !errors.Is(err, pay.ErrNoStore) {
		t.Fatalf("got %v, want ErrNoStore", err)
	}
}

func TestAFailedStoreIsNotAcknowledged(t *testing.T) {
	gateway := paytest.New("whsec_test")
	st := newStore()
	st.failApply = errors.New("the database is down")
	billing := pay.New(gateway, st, pay.WithSecrets("whsec_test"))

	now := time.Now()
	payload := gateway.Event("evt_1", "customer.subscription.updated", now, activeSub(), "7")

	if err := billing.Handle(nil, payload, gateway.Sign(payload, now)); err == nil {
		t.Fatal("a failed apply was reported as success, so Stripe would never retry it")
	}
}

func TestCheckoutRefusesAnUnattributablePurchase(t *testing.T) {
	gateway := paytest.New("whsec_test")
	billing := pay.New(gateway, newStore())

	// A checkout with no client reference completes, takes the money, and
	// arrives on the webhook attached to nobody.
	if _, err := billing.Checkout(nil, pay.CheckoutRequest{PriceID: "price_1"}); err == nil {
		t.Fatal("a checkout with no ClientReference was allowed")
	}
	if _, err := billing.Checkout(nil, pay.CheckoutRequest{ClientReference: "7"}); err == nil {
		t.Fatal("a checkout with no PriceID was allowed")
	}

	checkout, err := billing.Checkout(nil, pay.CheckoutRequest{PriceID: "price_1", ClientReference: "7"})
	if err != nil {
		t.Fatal(err)
	}
	if checkout.URL == "" {
		t.Error("no checkout URL")
	}
}

func TestWithNoGatewayEverythingRefusesAndNothingPanics(t *testing.T) {
	billing := pay.New(nil, newStore())

	if _, err := billing.Checkout(nil, pay.CheckoutRequest{PriceID: "p", ClientReference: "7"}); !errors.Is(err, pay.ErrNoGateway) {
		t.Errorf("Checkout: %v", err)
	}
	if _, err := billing.Portal(nil, "cus_1", "/"); !errors.Is(err, pay.ErrNoGateway) {
		t.Errorf("Portal: %v", err)
	}
	if _, err := billing.Subscription(nil, "sub_1"); !errors.Is(err, pay.ErrNoGateway) {
		t.Errorf("Subscription: %v", err)
	}
	if err := billing.Handle(nil, nil, ""); !errors.Is(err, pay.ErrNoGateway) {
		t.Errorf("Handle: %v", err)
	}
}

func TestEntitledUntilIsThePeriodEnd(t *testing.T) {
	sub := activeSub()
	// Somebody who cancels today keeps what they paid for until the period
	// ends.
	sub.CancelAtPeriodEnd = true
	until := sub.EntitledUntil()
	if until == nil || !until.Equal(*sub.CurrentPeriodEnd) {
		t.Errorf("EntitledUntil = %v, want the period end", until)
	}

	sub.Status = pay.StatusCanceled
	if sub.EntitledUntil() != nil {
		t.Error("a canceled subscription still entitled somebody")
	}

	// past_due earns but does not entitle, so it grants nothing here either.
	sub.Status = pay.StatusPastDue
	if sub.EntitledUntil() != nil {
		t.Error("a past_due subscription entitled somebody")
	}
}
