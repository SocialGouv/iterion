package main

import (
	"context"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/notify"
)

// launcher is the subset of LaunchClient the Coordinator needs, broken
// out so tests can inject a fake.
type launcher interface {
	Launch(ctx context.Context, transcript, latest, threadID, token string) (string, error)
}

// Coordinator is the platform-agnostic brain. It owns the per-thread
// consent + anonymisation state and decides, for each inbound post,
// whether to prompt for consent, stay silent, or launch a clarify run.
// It is driven by a ChannelDriver (for I/O) and a RelevanceFilter (for
// the cheap gate); both are injected so the logic is unit-testable
// without Mattermost.
type Coordinator struct {
	driver ChannelDriver
	filter RelevanceFilter
	launch launcher
	store  *ConsentStore

	mu        sync.Mutex
	anonymity map[string]*Anonymizer // threadID → anonymizer
	history   map[string][]Message   // threadID → observed messages (consenting + not; filtered at transcript time)
}

// NewCoordinator builds a Coordinator.
func NewCoordinator(driver ChannelDriver, filter RelevanceFilter, l launcher) *Coordinator {
	return &Coordinator{
		driver:    driver,
		filter:    filter,
		launch:    l,
		store:     NewConsentStore(),
		anonymity: make(map[string]*Anonymizer),
		history:   make(map[string][]Message),
	}
}

// threadKey is the stable per-thread map key.
func threadKey(t ThreadRef) string { return t.ChannelID + "/" + t.RootID }

func (c *Coordinator) anonymizerFor(key string) *Anonymizer {
	a := c.anonymity[key]
	if a == nil {
		a = NewAnonymizer()
		c.anonymity[key] = a
	}
	return a
}

// HandlePost is the core per-message decision. It returns true when it
// launched a clarify run (used by tests; the live loop ignores it).
//
// Order of operations:
//  1. Ignore the bot's own posts.
//  2. A mention activates the thread and prompts the author for consent
//     if they haven't decided yet.
//  3. Only act on activated threads.
//  4. Record the message in thread history (always — so chronology is
//     intact — but non-consenting authors are excluded at transcript
//     build time, never sent to the LLM).
//  5. Prompt any newly-seen, undecided participant for consent.
//  6. Skip the LLM unless the author has consented (we won't act on a
//     message we are not allowed to send).
//  7. Run the cheap relevance filter; only on a hit do we build the
//     anonymised transcript and launch a run.
func (c *Coordinator) HandlePost(ctx context.Context, p InboundPost) bool {
	if p.FromBot {
		return false
	}
	key := threadKey(p.Thread)

	if p.MentionsBot {
		c.store.Activate(p.Thread.RootID)
	}
	if !c.store.IsActive(p.Thread.RootID) {
		return false
	}

	c.mu.Lock()
	c.history[key] = append(c.history[key], Message{
		UserID: p.UserID, Text: p.Text, CreateAtMillis: p.CreateAtMillis,
	})
	hist := append([]Message(nil), c.history[key]...)
	anon := c.anonymizerFor(key)
	c.mu.Unlock()

	// Prompt any undecided participant once.
	if c.store.NeedsPrompt(p.Thread.RootID, p.UserID) {
		_ = c.driver.RequestConsent(ctx, p.Thread, p.UserID)
		c.store.MarkPrompted(p.Thread.RootID, p.UserID)
	}

	// We only act on messages we are permitted to send to the LLM.
	if !c.store.HasConsented(p.Thread.RootID, p.UserID) {
		return false
	}

	// Cheap relevance gate before the expensive run.
	consented := c.store.ConsentedSet(p.Thread.RootID)
	transcript := anon.Transcript(hist, consented)
	latest := strings.TrimSpace(p.Text)
	if !c.filter.ShouldRespond(transcript, latest) {
		return false
	}

	token := encodeToken(callbackToken{ChannelID: p.Thread.ChannelID, RootID: p.Thread.RootID})
	if _, err := c.launch.Launch(ctx, transcript, latest, p.Thread.RootID, token); err != nil {
		return false
	}
	return true
}

// HandleConsent records a consent button click.
func (c *Coordinator) HandleConsent(a ConsentAction) {
	c.store.Set(a.Thread.RootID, a.UserID, a.Granted)
}

// HandleCompletion posts the run's final answer back into the
// originating thread. Called by the HTTP callback handler with the
// payload iterion delivered. An empty final_answer (the facilitator
// chose silence) posts nothing.
func (c *Coordinator) HandleCompletion(ctx context.Context, payload notify.CompletionPayload) error {
	if strings.TrimSpace(payload.FinalAnswer) == "" {
		return nil
	}
	tok, err := decodeToken(payload.CallbackToken)
	if err != nil {
		return err
	}
	thread := ThreadRef(tok)
	return c.driver.PostReply(ctx, thread, payload.FinalAnswer)
}

// Run drives the driver's Listen loop until ctx is cancelled.
func (c *Coordinator) Run(ctx context.Context) error {
	posts, err := c.driver.Listen(ctx)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case p, ok := <-posts:
			if !ok {
				return nil
			}
			c.HandlePost(ctx, p)
		}
	}
}
