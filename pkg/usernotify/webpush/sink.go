package webpush

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	wp "github.com/SherClockHolmes/webpush-go"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/usernotify"
)

// pushTTL is how long a push service holds an undelivered message for an
// offline device — a day covers "answer it when you're back at your desk"
// without resurrecting stale prompts much later.
const pushTTL = 24 * time.Hour

// SinkOptions configures the web-push sink. The VAPID keypair must be
// shared by every server replica (it is the sender identity browsers pin
// at subscribe time).
type SinkOptions struct {
	VAPIDPublicKey  string
	VAPIDPrivateKey string
	// Subscriber is the VAPID contact (mailto: or https: URL) push services
	// may use to reach the operator.
	Subscriber string
	// HTTPClient overrides the client used to reach push services (tests).
	HTTPClient *http.Client
}

// Sink implements usernotify.Sink over Web Push.
type Sink struct {
	store  SubscriptionStore
	opts   SinkOptions
	logger *iterlog.Logger
}

func NewSink(store SubscriptionStore, opts SinkOptions, logger *iterlog.Logger) *Sink {
	if logger == nil {
		logger = iterlog.Nop()
	}
	return &Sink{store: store, opts: opts, logger: logger}
}

func (s *Sink) Name() string { return "webpush" }

// payload is what the service worker's `push` handler receives. Keep it
// small (push services cap the body around 4KB).
type payload struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Body  string `json:"body"`
	Link  string `json:"link"`
	Tag   string `json:"tag"`
}

// pushConcurrency bounds the parallel HTTP calls to push services — enough
// to make wall time ≈ one RTT for a typical fan-out without stampeding.
const pushConcurrency = 8

// Deliver pushes n to every registered browser of every recipient — one
// subscription query, then bounded-parallel HTTP sends (each is a blocking
// call to an external push service; serialized they can outlast the
// deliver budget). A push endpoint the service reports dead (404/410 Gone)
// is pruned. Deliver only errors when nothing could be delivered despite
// at least one live subscription — a recipient with no subscriptions is
// not a failure.
func (s *Sink) Deliver(ctx context.Context, n usernotify.Notification) error {
	body, err := json.Marshal(payload{
		Kind:  string(n.Kind),
		Title: n.Title,
		Body:  n.Body,
		Link:  n.Link,
		Tag:   n.Tag,
	})
	if err != nil {
		return fmt.Errorf("webpush: marshal payload: %w", err)
	}

	subs, err := s.store.ListForUsers(ctx, n.UserIDs)
	if err != nil {
		return fmt.Errorf("webpush: list subscriptions: %w", err)
	}
	if len(subs) == 0 {
		return nil
	}

	sem := make(chan struct{}, pushConcurrency)
	var wg sync.WaitGroup
	var delivered atomic.Int32
	for _, sub := range subs {
		wg.Add(1)
		go func(sub *Subscription) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := s.push(ctx, body, sub); err != nil {
				s.logger.Warn("webpush: push to %s (user %s): %v", redactEndpoint(sub.Endpoint), sub.UserID, err)
				return
			}
			delivered.Add(1)
		}(sub)
	}
	wg.Wait()
	if delivered.Load() == 0 {
		return fmt.Errorf("webpush: all %d push attempts failed for run %s", len(subs), n.RunID)
	}
	return nil
}

func (s *Sink) push(ctx context.Context, body []byte, sub *Subscription) error {
	// webpush-go wraps the message with bytes.NewBuffer (no copy) and then
	// appends a padding delimiter into it, mutating the slice's backing
	// array in place. Deliver fans the SAME body slice out to every
	// concurrent push goroutine, so without a per-call copy those in-place
	// writes race on the shared array (caught by -race in
	// TestSinkDeliverAndPrune). Give each send its own copy.
	msg := make([]byte, len(body))
	copy(msg, body)
	resp, err := wp.SendNotificationWithContext(ctx, msg, &wp.Subscription{
		Endpoint: sub.Endpoint,
		Keys:     wp.Keys{P256dh: sub.P256dh, Auth: sub.Auth},
	}, &wp.Options{
		HTTPClient:      s.opts.HTTPClient,
		Subscriber:      s.opts.Subscriber,
		VAPIDPublicKey:  s.opts.VAPIDPublicKey,
		VAPIDPrivateKey: s.opts.VAPIDPrivateKey,
		TTL:             int(pushTTL / time.Second),
		Urgency:         wp.UrgencyHigh,
	})
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		// The browser unsubscribed or the registration expired — the
		// endpoint is permanently dead, drop it.
		if err := s.store.Prune(context.WithoutCancel(ctx), sub.Endpoint); err != nil {
			s.logger.Warn("webpush: prune dead endpoint %s: %v", redactEndpoint(sub.Endpoint), err)
		}
		return fmt.Errorf("push service returned %d (subscription pruned)", resp.StatusCode)
	case resp.StatusCode >= 400:
		return fmt.Errorf("push service returned %d", resp.StatusCode)
	}
	if err := s.store.Touch(context.WithoutCancel(ctx), sub.Endpoint, time.Now().UTC()); err != nil {
		s.logger.Warn("webpush: touch endpoint %s: %v", redactEndpoint(sub.Endpoint), err)
	}
	return nil
}

// redactEndpoint trims the capability token from a push endpoint before it
// reaches logs — the full URL suffices to send pushes to that browser.
func redactEndpoint(endpoint string) string {
	const keep = 40
	if len(endpoint) <= keep {
		return endpoint
	}
	return endpoint[:keep] + "…"
}

// GenerateVAPIDKeys mints a fresh VAPID keypair (private, public) for the
// `iterion server webpush-keys` helper.
func GenerateVAPIDKeys() (privateKey, publicKey string, err error) {
	return wp.GenerateVAPIDKeys()
}
