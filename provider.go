package pay

import (
	"log/slog"
	"strings"

	"github.com/lemmego/api/app"
)

// Provider builds the application's Billing.
//
//	&pay.Provider{
//	    Gateway: stripe.New(stripe.Config{
//	        SecretKey: config.MustEnv("STRIPE_SECRET_KEY", ""),
//	    }),
//	    Store:   &billing.Store{},
//	    Secrets: []string{config.MustEnv("STRIPE_WEBHOOK_SECRET", "")},
//	}
//
// With no Gateway, every paid path refuses with ErrNoGateway and the rest of
// the application runs. That default matters for a development boot and for
// tests: an application whose home page will not render without Stripe
// credentials is one nobody can contribute to.
type Provider struct {
	// Gateway is the payment processor. Nil disables paid features.
	Gateway Gateway
	// Revenue is the read-only revenue capability, when the gateway has one.
	// Separate from Gateway so an application can charge without being able
	// to read anybody's books.
	Revenue RevenueReader
	// Store builds the persistence webhook-driven state lands in. Required
	// whenever Gateway is set: a gateway with no store takes money and
	// records nothing.
	//
	// A function, like seo.Provider.Sources, because the provider list is
	// built before the container exists and a store needs a connection.
	Store func(a app.App) Store
	// Secrets are the webhook signing secrets. More than one during a
	// rotation.
	Secrets []string
	// Livemode declares which mode this process is in. Left unset it is
	// derived from the gateway, which is strictly better than configuring it:
	// a flag that can disagree with the key in use eventually does, and the
	// failure is a test-mode event granting a real subscription.
	Livemode *bool
}

func (p *Provider) Provide(a app.App) error {
	store := p.store(a)

	if p.Gateway == nil {
		slog.Info("pay: no gateway is configured, so paid features are disabled")
		billing := New(nil, store)
		a.AddService(billing)
		SetGlobal(billing)
		return nil
	}

	if store == nil {
		// A warning and not an error, because refusing to boot takes the
		// whole application down over a feature most requests never touch.
		// The webhook endpoint then answers 500 and Stripe retries, which is
		// the correct behaviour for "the event was real and we did not record
		// it" — the events wait rather than being lost.
		slog.Error("pay: a gateway is configured with no store, so verified webhooks cannot be recorded")
	}

	live := false
	if p.Livemode != nil {
		live = *p.Livemode
	} else if detector, ok := p.Gateway.(interface{ Livemode() bool }); ok {
		live = detector.Livemode()
	}

	if len(p.Secrets) == 0 || allEmpty(p.Secrets) {
		// Stated loudly. With no secret, VerifySignature refuses everything,
		// so the endpoint is closed rather than open — but the symptom is
		// "subscriptions never activate", which is a long way from its cause.
		slog.Error("pay: no webhook signing secret is configured, so every delivery will be refused")
	}

	billing := New(p.Gateway, store,
		WithSecrets(p.Secrets...),
		WithRevenueReader(p.Revenue),
		WithLivemode(live),
	)
	a.AddService(billing)
	SetGlobal(billing)
	return nil
}

func (p *Provider) store(a app.App) Store {
	if p.Store == nil {
		return nil
	}
	return p.Store(a)
}

func allEmpty(values []string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return false
		}
	}
	return true
}
