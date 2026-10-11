// Package pay charges money and records what was bought.
//
// It is deliberately a thin layer over one hosted checkout and one webhook
// endpoint, because the expensive mistakes in billing are not in the API calls
// — they are in what the application believes afterwards. The three that cost
// real money, and what this package does about each:
//
// Entitlement comes from the webhook, never from the redirect. A browser
// returning to a success URL proves only that a browser was sent there: the
// person can close the tab, the redirect can be lost, and the URL can be
// visited again tomorrow by anyone who remembers it. The webhook is signed,
// retried until acknowledged, and is the only thing Handle will write from.
//
// A delivery arrives more than once, and out of order. Stripe retries until
// it gets a 2xx and promises no ordering at all, so Store.Seen deduplicates
// on the event id and Store.Apply has to let an older event lose against a
// newer one — as a condition on the write, not a read followed by a write.
//
// A signature is checked over the exact bytes received, with a timestamp
// inside the MAC and a tolerance around it. Without the tolerance a signature
// captured once is valid forever; without the raw bytes the check passes only
// by luck, since decoding and re-encoding the same JSON changes the
// signature.
//
// Reading somebody else's revenue is a separate interface, deliberately.
// RevenueReader is not part of Gateway because charging a customer and reading
// a founder's books are different authorisations with different blast radii,
// and an application should be able to do the first without being able to do
// the second.
//
// What is not here: refunds, invoices, metered usage, tax, one-off charges,
// a cancel flow of our own, and any local copy of a price. Each of them is
// either the gateway's hosted page doing a better job than we would, or a
// Stripe endpoint whose presence here would make the interface
// Stripe-shaped — at which point it is no longer an abstraction, just a
// longer way to spell the SDK.
//
//	&pay.Provider{
//	    Gateway: stripe.New(stripe.Config{SecretKey: key}),
//	    Store:   &billing.Store{},
//	    Secrets: []string{webhookSecret},
//	}
//
//	r.Post("/webhooks/stripe", pay.WebhookHandler())
package pay
