package slack

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/deasa/YNAB_AutoCategorizer/store"
)

const (
	testChannelID   = "C_FIN"
	testChannelName = "finbot-finances"
	userBrendan     = "U_BRENDAN"
	userWife        = "U_WIFE"
)

// fakeSocket feeds events and records acks in order.
type fakeSocket struct {
	events chan socketmode.Event
	acked  chan string
}

func newFakeSocket() *fakeSocket {
	return &fakeSocket{events: make(chan socketmode.Event, 16), acked: make(chan string, 16)}
}

func (s *fakeSocket) Events() <-chan socketmode.Event { return s.events }
func (s *fakeSocket) Ack(req socketmode.Request) error {
	s.acked <- req.EnvelopeID
	return nil
}
func (s *fakeSocket) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// fakeAPI serves pages of channels and records posts and updates.
type fakeAPI struct {
	pages   [][]slackapi.Channel
	err     error
	mu      sync.Mutex
	posts   []string
	updates []string
}

func (a *fakeAPI) GetConversationsForUserContext(_ context.Context, p *slackapi.GetConversationsForUserParameters) ([]slackapi.Channel, string, error) {
	if a.err != nil {
		return nil, "", a.err
	}
	i := 0
	if p.Cursor != "" {
		i = int(p.Cursor[0] - '0')
	}
	next := ""
	if i+1 < len(a.pages) {
		next = string(rune('0' + i + 1))
	}
	return a.pages[i], next, nil
}

func (a *fakeAPI) PostMessageContext(_ context.Context, ch string, _ ...slackapi.MsgOption) (string, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.posts = append(a.posts, ch)
	return ch, "200.1", nil
}

func (a *fakeAPI) UpdateMessageContext(_ context.Context, ch, ts string, _ ...slackapi.MsgOption) (string, string, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.updates = append(a.updates, ch+"/"+ts)
	return ch, ts, "", nil
}

func channel(id, name string) slackapi.Channel {
	var c slackapi.Channel
	c.ID, c.Name = id, name
	return c
}

func goodAPI() *fakeAPI {
	return &fakeAPI{pages: [][]slackapi.Channel{
		{channel("C_OTHER", "random-private")},
		{channel(testChannelID, testChannelName)},
	}}
}

func openStore(t *testing.T, path string) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func newStore(t *testing.T) (*store.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "finbot.db")
	db := openStore(t, path)
	t.Cleanup(func() { db.Close() })
	err := db.UpsertUsers(context.Background(), []store.User{
		{SlackUserID: userBrendan, Label: store.UserBrendan},
		{SlackUserID: userWife, Label: store.UserWife},
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return db, path
}

// newClient builds a client with the channel resolved.
func newClient(t *testing.T, st Store, sock Socket, h Handlers) *Client {
	t.Helper()
	c := New(Config{Channel: testChannelName}, goodAPI(), sock, st, h, nil)
	if err := c.SelfCheck(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c
}

type msgOpts struct {
	eventID, user, channel, subtype, botID, threadTS string
}

// msgEvent builds an events_api envelope the way socketmode parses one.
func msgEvent(t *testing.T, o msgOpts) socketmode.Event {
	t.Helper()
	inner := map[string]any{"type": "message", "user": o.user, "channel": o.channel, "ts": "300.1",
		"text": "hi", "channel_type": "group"}
	if o.subtype != "" {
		inner["subtype"] = o.subtype
	}
	if o.botID != "" {
		inner["bot_id"] = o.botID
	}
	if o.threadTS != "" {
		inner["thread_ts"] = o.threadTS
	}
	payload, err := json.Marshal(map[string]any{"type": "event_callback", "event_id": o.eventID, "event": inner})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := slackevents.ParseEvent(payload, slackevents.OptionNoVerifyToken())
	if err != nil {
		t.Fatal(err)
	}
	return socketmode.Event{Type: socketmode.EventTypeEventsAPI, Data: ev,
		Request: &socketmode.Request{Type: "events_api", EnvelopeID: "env-" + o.eventID, Payload: payload}}
}

// clickEvent builds an interactive block_actions envelope.
func clickEvent(t *testing.T, user, ch, messageTS, actionTS string) socketmode.Event {
	t.Helper()
	payload := []byte(`{"type":"block_actions","user":{"id":"` + user + `"},` +
		`"container":{"type":"message","message_ts":"` + messageTS + `","channel_id":"` + ch + `"},` +
		`"channel":{"id":"` + ch + `"},` +
		`"actions":[{"type":"button","action_id":"pick","block_id":"q","value":"txn:1|cat:2","action_ts":"` + actionTS + `"}]}`)
	var cb slackapi.InteractionCallback
	if err := json.Unmarshal(payload, &cb); err != nil {
		t.Fatal(err)
	}
	return socketmode.Event{Type: socketmode.EventTypeInteractive, Data: cb,
		Request: &socketmode.Request{Type: "interactive", EnvelopeID: "env-click-" + actionTS, Payload: payload}}
}

func runClient(t *testing.T, c *Client) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run did not return")
		}
	}
}

func waitFor[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

func TestAckBeforeBlockingHandler(t *testing.T) {
	db, _ := newStore(t)
	sock := newFakeSocket()
	release := make(chan struct{})
	started := make(chan string, 4)
	c := newClient(t, db, sock, Handlers{Message: func(_ context.Context, m Message) {
		started <- m.EventID
		<-release
	}})
	stop := runClient(t, c)
	defer stop()

	sock.events <- msgEvent(t, msgOpts{eventID: "Ev1", user: userBrendan, channel: testChannelID})
	if got := waitFor(t, sock.acked, "first ack"); got != "env-Ev1" {
		t.Fatalf("acked %q", got)
	}
	waitFor(t, started, "first handler")

	// The first handler is still blocked: the next envelope is acked and
	// handled anyway.
	sock.events <- msgEvent(t, msgOpts{eventID: "Ev2", user: userWife, channel: testChannelID})
	if got := waitFor(t, sock.acked, "second ack"); got != "env-Ev2" {
		t.Fatalf("acked %q", got)
	}
	waitFor(t, started, "second handler")
	close(release)
}

func TestFilterAndAuthorize(t *testing.T) {
	db, _ := newStore(t)
	tests := []struct {
		name string
		evt  func(t *testing.T) socketmode.Event
		want bool
	}{
		{"plain message", func(t *testing.T) socketmode.Event {
			return msgEvent(t, msgOpts{eventID: "E1", user: userBrendan, channel: testChannelID})
		}, true},
		{"thread reply", func(t *testing.T) socketmode.Event {
			return msgEvent(t, msgOpts{eventID: "E2", user: userWife, channel: testChannelID, threadTS: "100.1"})
		}, true},
		{"thread_broadcast passes", func(t *testing.T) socketmode.Event {
			return msgEvent(t, msgOpts{eventID: "E3", user: userBrendan, channel: testChannelID, subtype: "thread_broadcast", threadTS: "100.1"})
		}, true},
		{"bot message", func(t *testing.T) socketmode.Event {
			return msgEvent(t, msgOpts{eventID: "E4", user: userBrendan, channel: testChannelID, botID: "B1"})
		}, false},
		{"edited message", func(t *testing.T) socketmode.Event {
			return msgEvent(t, msgOpts{eventID: "E5", user: userBrendan, channel: testChannelID, subtype: "message_changed"})
		}, false},
		{"deleted message", func(t *testing.T) socketmode.Event {
			return msgEvent(t, msgOpts{eventID: "E6", user: userBrendan, channel: testChannelID, subtype: "message_deleted"})
		}, false},
		{"unknown user", func(t *testing.T) socketmode.Event {
			return msgEvent(t, msgOpts{eventID: "E7", user: "U_STRANGER", channel: testChannelID})
		}, false},
		{"wrong channel", func(t *testing.T) socketmode.Event {
			return msgEvent(t, msgOpts{eventID: "E8", user: userBrendan, channel: "C_OTHER"})
		}, false},
		{"click", func(t *testing.T) socketmode.Event {
			return clickEvent(t, userWife, testChannelID, "100.1", "400.1")
		}, true},
		{"click by unknown user", func(t *testing.T) socketmode.Event {
			return clickEvent(t, "U_STRANGER", testChannelID, "100.1", "400.2")
		}, false},
		{"click in wrong channel", func(t *testing.T) socketmode.Event {
			return clickEvent(t, userBrendan, "C_OTHER", "100.1", "400.3")
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got bool
			c := newClient(t, db, newFakeSocket(), Handlers{
				Message: func(context.Context, Message) { got = true },
				Click:   func(context.Context, Click) { got = true },
			})
			c.handle(context.Background(), tc.evt(t))
			if got != tc.want {
				t.Fatalf("handled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCleanEventFields(t *testing.T) {
	db, _ := newStore(t)
	var m Message
	var k Click
	c := newClient(t, db, newFakeSocket(), Handlers{
		Message: func(_ context.Context, got Message) { m = got },
		Click:   func(_ context.Context, got Click) { k = got },
	})
	c.handle(context.Background(), msgEvent(t, msgOpts{eventID: "F1", user: userWife, channel: testChannelID, threadTS: "100.1"}))
	if m.EventID != "F1" || m.User.Label != store.UserWife || m.ThreadTS != "100.1" || m.TS != "300.1" || m.Text != "hi" {
		t.Fatalf("message = %+v", m)
	}
	c.handle(context.Background(), clickEvent(t, userBrendan, testChannelID, "100.1", "400.9"))
	if k.User.Label != store.UserBrendan || k.MessageTS != "100.1" || k.ActionTS != "400.9" ||
		k.ActionID != "pick" || k.Value != "txn:1|cat:2" || k.Channel != testChannelID {
		t.Fatalf("click = %+v", k)
	}
}

func TestRedeliveryHandledOnce(t *testing.T) {
	_, path := newStore(t) // seeds users; its handle is closed at cleanup
	var msgs, clicks atomic.Int32
	h := Handlers{
		Message: func(context.Context, Message) { msgs.Add(1) },
		Click:   func(context.Context, Click) { clicks.Add(1) },
	}
	msg := msgEvent(t, msgOpts{eventID: "Ev_dup", user: userBrendan, channel: testChannelID})
	click := clickEvent(t, userWife, testChannelID, "100.1", "500.1")

	db1 := openStore(t, path)
	c1 := newClient(t, db1, newFakeSocket(), h)
	for range 2 {
		c1.handle(context.Background(), msg)
		c1.handle(context.Background(), click)
	}
	if err := db1.Close(); err != nil {
		t.Fatal(err)
	}

	db2 := openStore(t, path)
	defer db2.Close()
	c2 := newClient(t, db2, newFakeSocket(), h)
	c2.handle(context.Background(), msg)
	c2.handle(context.Background(), click)

	if msgs.Load() != 1 || clicks.Load() != 1 {
		t.Fatalf("messages handled %d, clicks handled %d; want 1 each", msgs.Load(), clicks.Load())
	}

	// A second click on the same message (a new action_ts) is a new event.
	c2.handle(context.Background(), clickEvent(t, userWife, testChannelID, "100.1", "500.2"))
	if clicks.Load() != 2 {
		t.Fatalf("clicks handled %d, want 2", clicks.Load())
	}
}

func TestSelfCheck(t *testing.T) {
	t.Run("channel on a later page resolves", func(t *testing.T) {
		c := New(Config{Channel: testChannelName}, goodAPI(), newFakeSocket(), nil, Handlers{}, nil)
		if err := c.SelfCheck(context.Background()); err != nil {
			t.Fatal(err)
		}
		if c.ChannelID() != testChannelID {
			t.Fatalf("channel id %q", c.ChannelID())
		}
	})
	t.Run("channel not listed fails", func(t *testing.T) {
		api := &fakeAPI{pages: [][]slackapi.Channel{{channel("C_OTHER", "random-private")}, {channel("C_X", "x")}}}
		c := New(Config{Channel: testChannelName}, api, newFakeSocket(), nil, Handlers{}, nil)
		if err := c.SelfCheck(context.Background()); !errors.Is(err, ErrChannelNotFound) {
			t.Fatalf("err = %v, want ErrChannelNotFound", err)
		}
		// Run refuses to start without the channel.
		if err := c.Run(context.Background()); !errors.Is(err, ErrChannelNotFound) {
			t.Fatalf("Run err = %v, want ErrChannelNotFound", err)
		}
		if _, err := c.Post(context.Background()); err == nil {
			t.Fatal("Post succeeded without a resolved channel")
		}
	})
	t.Run("API error is returned redacted", func(t *testing.T) {
		api := &fakeAPI{err: errors.New("bad xoxb-secret")}
		c := New(Config{Channel: testChannelName}, api, newFakeSocket(), nil, Handlers{}, nil)
		c.secrets = []string{"xoxb-secret"}
		err := c.SelfCheck(context.Background())
		if err == nil || errors.Is(err, ErrChannelNotFound) || strings.Contains(err.Error(), "xoxb-secret") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestPanickingHandlerDoesNotStopLaterEvents(t *testing.T) {
	db, _ := newStore(t)
	sock := newFakeSocket()
	handled := make(chan string, 4)
	c := newClient(t, db, sock, Handlers{Message: func(_ context.Context, m Message) {
		if m.EventID == "EvPanic" {
			panic("boom")
		}
		handled <- m.EventID
	}})
	stop := runClient(t, c)
	defer stop()

	sock.events <- msgEvent(t, msgOpts{eventID: "EvPanic", user: userBrendan, channel: testChannelID})
	waitFor(t, sock.acked, "panic ack")
	sock.events <- msgEvent(t, msgOpts{eventID: "EvAfter", user: userBrendan, channel: testChannelID})
	if got := waitFor(t, handled, "later event"); got != "EvAfter" {
		t.Fatalf("handled %q", got)
	}
}

func TestPostAndUpdateUseResolvedChannel(t *testing.T) {
	api := goodAPI()
	c := New(Config{Channel: testChannelName}, api, newFakeSocket(), nil, Handlers{}, nil)
	if err := c.SelfCheck(context.Background()); err != nil {
		t.Fatal(err)
	}
	ts, err := c.Post(context.Background(), slackapi.MsgOptionText("q", false))
	if err != nil || ts != "200.1" {
		t.Fatalf("Post = %q, %v", ts, err)
	}
	if err := c.Update(context.Background(), ts, slackapi.MsgOptionText("a", false)); err != nil {
		t.Fatal(err)
	}
	if len(api.posts) != 1 || api.posts[0] != testChannelID || len(api.updates) != 1 || api.updates[0] != testChannelID+"/200.1" {
		t.Fatalf("posts %v updates %v", api.posts, api.updates)
	}
}

func TestNewFromTokensValidates(t *testing.T) {
	if _, err := NewFromTokens("xoxb-a", "xoxb-b", Config{Channel: "x"}, nil, Handlers{}, nil); err == nil {
		t.Fatal("accepted a bot token as the app token")
	}
	if _, err := NewFromTokens("xapp-a", "xapp-b", Config{Channel: "x"}, nil, Handlers{}, nil); err == nil {
		t.Fatal("accepted an app token as the bot token")
	}
	if _, err := NewFromTokens("xapp-a", "xoxb-b", Config{}, nil, Handlers{}, nil); err == nil {
		t.Fatal("accepted an empty channel")
	}
	if _, err := NewFromTokens("xapp-a", "xoxb-b", Config{Channel: "x"}, nil, Handlers{}, nil); err != nil {
		t.Fatal(err)
	}
}
