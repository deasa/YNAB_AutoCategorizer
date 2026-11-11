// Package slack is finbot's Slack Socket Mode client. It connects outbound
// over a websocket, acks every envelope before doing anything else, drops
// traffic that isn't from an allowed user in the configured private channel,
// drops redeliveries, and hands clean messages and button clicks to handlers.
//
// The status/ package keeps its own small Slack client for the heartbeat
// message; this one is for interactive traffic.
//
// Nothing logged here carries a token or an id.
package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/deasa/YNAB_AutoCategorizer/store"
)

// API is the part of the Slack Web API the client uses, met by *slack.Client.
type API interface {
	GetConversationsForUserContext(ctx context.Context, params *slackapi.GetConversationsForUserParameters) ([]slackapi.Channel, string, error)
	PostMessageContext(ctx context.Context, channelID string, options ...slackapi.MsgOption) (string, string, error)
	UpdateMessageContext(ctx context.Context, channelID, timestamp string, options ...slackapi.MsgOption) (string, string, string, error)
}

// Socket is a Socket Mode connection: a stream of events, a way to ack an
// envelope, and a run loop that reconnects on its own until ctx ends.
type Socket interface {
	Events() <-chan socketmode.Event
	Ack(req socketmode.Request) error
	Run(ctx context.Context) error
}

// Store is what the client needs from the database.
type Store interface {
	SeenSlackEvent(ctx context.Context, key string, now time.Time) (duplicate bool, err error)
	UserBySlackID(ctx context.Context, slackUserID string) (store.User, bool, error)
}

// Message is a message from an allowed user in the configured channel: a new
// top-level message, a thread reply, or a reply also sent to the channel.
type Message struct {
	EventID  string
	User     store.User
	Channel  string
	TS       string
	ThreadTS string // empty unless it's a thread reply
	Text     string
	Subtype  string // "" or "thread_broadcast"
}

// Click is a Block Kit button click by an allowed user on a message in the
// configured channel.
type Click struct {
	User      store.User
	Channel   string
	MessageTS string // the message holding the button
	ActionTS  string
	ActionID  string
	BlockID   string
	Value     string
}

// Handlers receive clean traffic. A nil handler drops that kind. Each call
// runs on its own goroutine with the client's run ctx; a panic is recovered.
type Handlers struct {
	Message func(ctx context.Context, m Message)
	Click   func(ctx context.Context, c Click)
}

// Config names the channel finbot listens and posts in: its name (without
// "#") or its id.
type Config struct {
	Channel string
}

// maxInFlight bounds how many handlers run at once.
const maxInFlight = 16

// Client is the Socket Mode client. Build it with New or NewFromTokens, call
// SelfCheck at startup, then Run.
type Client struct {
	cfg      Config
	api      API
	sock     Socket
	st       Store
	h        Handlers
	log      *slog.Logger
	now      func() time.Time
	secrets  []string // redacted from error text
	slots    chan struct{}
	wg       sync.WaitGroup
	mu       sync.RWMutex
	chanID   string
	chanName string
}

// New builds a client over the given API, socket and store.
func New(cfg Config, api API, sock Socket, st Store, h Handlers, log *slog.Logger) *Client {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Client{
		cfg: cfg, api: api, sock: sock, st: st, h: h, log: log,
		now:   time.Now,
		slots: make(chan struct{}, maxInFlight),
	}
}

// NewFromTokens builds a client on slack-go with the app-level token (xapp-,
// for Socket Mode) and the bot token (xoxb-, for the Web API).
func NewFromTokens(appToken, botToken string, cfg Config, st Store, h Handlers, log *slog.Logger) (*Client, error) {
	if !strings.HasPrefix(appToken, "xapp-") {
		return nil, errors.New("slack: app token must be an app-level token (xapp-)")
	}
	if !strings.HasPrefix(botToken, "xoxb-") {
		return nil, errors.New("slack: bot token must be a bot token (xoxb-)")
	}
	if cfg.Channel == "" {
		return nil, errors.New("slack: channel is not set")
	}
	api := slackapi.New(botToken, slackapi.OptionAppLevelToken(appToken))
	c := New(cfg, api, &socketModeAdapter{c: socketmode.New(api)}, st, h, log)
	c.secrets = []string{appToken, botToken}
	return c, nil
}

// socketModeAdapter fits *socketmode.Client to Socket.
type socketModeAdapter struct{ c *socketmode.Client }

func (a *socketModeAdapter) Events() <-chan socketmode.Event  { return a.c.Events }
func (a *socketModeAdapter) Ack(req socketmode.Request) error { return a.c.Ack(req) }
func (a *socketModeAdapter) Run(ctx context.Context) error    { return a.c.RunContext(ctx) }

// ErrChannelNotFound is returned by SelfCheck when the configured channel is
// not among the private channels the bot is a member of.
var ErrChannelNotFound = errors.New("slack: configured channel not found among the bot's private channels (is the bot a member?)")

// SelfCheck resolves the configured channel through users.conversations.
// The caller alerts on an error; Run refuses to start without a resolved
// channel.
func (c *Client) SelfCheck(ctx context.Context) error {
	params := &slackapi.GetConversationsForUserParameters{
		Types:           []string{"private_channel"},
		Limit:           200,
		ExcludeArchived: true,
	}
	for {
		chans, next, err := c.api.GetConversationsForUserContext(ctx, params)
		if err != nil {
			return fmt.Errorf("slack: self-check: %s", c.redact(err))
		}
		for _, ch := range chans {
			if ch.ID == c.cfg.Channel || (ch.Name != "" && ch.Name == strings.TrimPrefix(c.cfg.Channel, "#")) {
				c.mu.Lock()
				c.chanID, c.chanName = ch.ID, ch.Name
				c.mu.Unlock()
				return nil
			}
		}
		if next == "" {
			return ErrChannelNotFound
		}
		params.Cursor = next
	}
}

// ChannelID is the resolved channel id, empty before a successful SelfCheck.
func (c *Client) ChannelID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.chanID
}

// Post posts a message to the channel and returns its ts.
func (c *Client) Post(ctx context.Context, opts ...slackapi.MsgOption) (ts string, err error) {
	ch := c.ChannelID()
	if ch == "" {
		return "", errors.New("slack: post: channel not resolved; run SelfCheck first")
	}
	_, ts, err = c.api.PostMessageContext(ctx, ch, opts...)
	if err != nil {
		return "", fmt.Errorf("slack: post: %s", c.redact(err))
	}
	return ts, nil
}

// Update replaces the message at ts in the channel.
func (c *Client) Update(ctx context.Context, ts string, opts ...slackapi.MsgOption) error {
	ch := c.ChannelID()
	if ch == "" {
		return errors.New("slack: update: channel not resolved; run SelfCheck first")
	}
	if _, _, _, err := c.api.UpdateMessageContext(ctx, ch, ts, opts...); err != nil {
		return fmt.Errorf("slack: update: %s", c.redact(err))
	}
	return nil
}

// Run connects and dispatches events until ctx ends, then waits for running
// handlers. It runs SelfCheck first if that hasn't succeeded yet. A cancelled
// ctx returns nil.
func (c *Client) Run(ctx context.Context) error {
	if c.ChannelID() == "" {
		if err := c.SelfCheck(ctx); err != nil {
			return err
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- c.sock.Run(runCtx) }()
	defer c.wg.Wait()
	events := c.sock.Events()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-runErr:
			if ctx.Err() != nil {
				return nil
			}
			if err == nil {
				err = errors.New("socket closed")
			}
			return fmt.Errorf("slack: socket mode: %s", c.redact(err))
		case evt, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			c.dispatch(runCtx, evt)
		}
	}
}

// dispatch acks the envelope, then hands the event to a goroutine.
func (c *Client) dispatch(ctx context.Context, evt socketmode.Event) {
	if evt.Request != nil && evt.Request.EnvelopeID != "" {
		if err := c.sock.Ack(*evt.Request); err != nil {
			c.log.Warn("slack ack failed", "type", string(evt.Type), "err", c.redact(err))
		}
	}
	switch evt.Type {
	case socketmode.EventTypeEventsAPI, socketmode.EventTypeInteractive:
	case socketmode.EventTypeConnecting, socketmode.EventTypeConnected, socketmode.EventTypeHello:
		c.log.Debug("slack socket", "state", string(evt.Type))
		return
	case socketmode.EventTypeConnectionError, socketmode.EventTypeInvalidAuth, socketmode.EventTypeIncomingError,
		socketmode.EventTypeErrorWriteFailed, socketmode.EventTypeErrorBadMessage, socketmode.EventTypeDisconnect:
		c.log.Warn("slack socket", "state", string(evt.Type))
		return
	default:
		c.log.Debug("slack event ignored", "type", string(evt.Type))
		return
	}
	select {
	case c.slots <- struct{}{}:
	case <-ctx.Done():
		return
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer func() { <-c.slots }()
		defer func() {
			if r := recover(); r != nil {
				c.log.Error("slack handler panicked", "type", string(evt.Type),
					"stack", c.redact(errors.New(string(debug.Stack()))))
			}
		}()
		c.handle(ctx, evt)
	}()
}

// handle filters, authorizes and de-duplicates one event, then calls its handler.
func (c *Client) handle(ctx context.Context, evt socketmode.Event) {
	switch evt.Type {
	case socketmode.EventTypeEventsAPI:
		m, reason := c.message(ctx, evt.Data)
		if reason != "" {
			c.drop("message", reason)
			return
		}
		if c.h.Message == nil || !c.firstSeen(ctx, "event:"+m.EventID) {
			return
		}
		c.h.Message(ctx, m)
	case socketmode.EventTypeInteractive:
		k, reason := c.click(ctx, evt.Data)
		if reason != "" {
			c.drop("click", reason)
			return
		}
		if c.h.Click == nil || !c.firstSeen(ctx, "click:"+k.MessageTS+":"+k.ActionTS) {
			return
		}
		c.h.Click(ctx, k)
	}
}

func (c *Client) drop(kind, reason string) {
	c.log.Debug("slack dropped", "kind", kind, "reason", reason)
}

// firstSeen records key and reports whether this is its first delivery. A
// store error drops the event rather than risk handling it twice.
func (c *Client) firstSeen(ctx context.Context, key string) bool {
	dup, err := c.st.SeenSlackEvent(ctx, key, c.now())
	if err != nil {
		c.log.Error("slack dedupe failed; event dropped", "err", c.redact(err))
		return false
	}
	if dup {
		c.drop("event", "redelivery")
	}
	return !dup
}

// message turns an Events API event into a Message, or gives a drop reason.
func (c *Client) message(ctx context.Context, data any) (Message, string) {
	var outer slackevents.EventsAPIEvent
	switch d := data.(type) {
	case slackevents.EventsAPIEvent:
		outer = d
	case *slackevents.EventsAPIEvent:
		outer = *d
	default:
		return Message{}, "unparsed event"
	}
	var eventID string
	switch cb := outer.Data.(type) {
	case *slackevents.EventsAPICallbackEvent:
		eventID = cb.EventID
	case slackevents.EventsAPICallbackEvent:
		eventID = cb.EventID
	}
	if eventID == "" {
		return Message{}, "no event id"
	}
	var ev *slackevents.MessageEvent
	switch d := outer.InnerEvent.Data.(type) {
	case *slackevents.MessageEvent:
		ev = d
	case slackevents.MessageEvent:
		ev = &d
	default:
		return Message{}, "not a message"
	}
	if ev.BotID != "" {
		return Message{}, "bot message"
	}
	if ev.SubType != "" && ev.SubType != "thread_broadcast" {
		return Message{}, "subtype " + ev.SubType
	}
	u, reason := c.authorize(ctx, ev.User, ev.Channel)
	if reason != "" {
		return Message{}, reason
	}
	return Message{
		EventID: eventID, User: u, Channel: ev.Channel, TS: ev.TimeStamp,
		ThreadTS: ev.ThreadTimeStamp, Text: ev.Text, Subtype: ev.SubType,
	}, ""
}

// click turns an interactive payload into a Click, or gives a drop reason.
func (c *Client) click(ctx context.Context, data any) (Click, string) {
	var cb slackapi.InteractionCallback
	switch d := data.(type) {
	case slackapi.InteractionCallback:
		cb = d
	case *slackapi.InteractionCallback:
		cb = *d
	default:
		return Click{}, "unparsed interaction"
	}
	if cb.Type != slackapi.InteractionTypeBlockActions {
		return Click{}, "interaction " + string(cb.Type)
	}
	if len(cb.ActionCallback.BlockActions) == 0 || cb.ActionCallback.BlockActions[0] == nil {
		return Click{}, "no action"
	}
	a := cb.ActionCallback.BlockActions[0]
	ch := cb.Container.ChannelID
	if ch == "" {
		ch = cb.Channel.ID
	}
	actionTS := a.ActionTs
	if actionTS == "" {
		actionTS = cb.ActionTs
	}
	if cb.Container.MessageTs == "" || actionTS == "" {
		return Click{}, "no message or action ts"
	}
	u, reason := c.authorize(ctx, cb.User.ID, ch)
	if reason != "" {
		return Click{}, reason
	}
	return Click{
		User: u, Channel: ch, MessageTS: cb.Container.MessageTs, ActionTS: actionTS,
		ActionID: a.ActionID, BlockID: a.BlockID, Value: a.Value,
	}, ""
}

// authorize accepts only a known user in the configured channel. Anyone else
// gets no effect and no reply.
func (c *Client) authorize(ctx context.Context, userID, channelID string) (store.User, string) {
	if want := c.ChannelID(); want == "" || channelID != want {
		return store.User{}, "wrong channel"
	}
	if userID == "" {
		return store.User{}, "no user"
	}
	u, ok, err := c.st.UserBySlackID(ctx, userID)
	if err != nil {
		c.log.Error("slack user lookup failed", "err", c.redact(err))
		return store.User{}, "user lookup failed"
	}
	if !ok {
		return store.User{}, "unknown user"
	}
	return u, ""
}

// redact strips the tokens and the channel id from error text.
func (c *Client) redact(err error) string {
	s := err.Error()
	for _, sec := range c.secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "<redacted>")
		}
	}
	if ch := c.ChannelID(); ch != "" {
		s = strings.ReplaceAll(s, ch, "<channel>")
	}
	return s
}
