// Command slack is a throwaway spike (M0-3) proving the Slack building blocks finbot needs:
// Socket Mode, posting to the private channel, Block Kit button clicks, threaded replies,
// Slack user id → identity, click latency and reconnect behaviour.
//
// All log output is redacted (ids, emails, names, URLs) so it can be pasted into docs.
//
//	go run .                      # post the TEST question and listen for 20 minutes
//	go run . -ts 1234.5678        # reuse an already-posted TEST message instead of posting
//	kill -USR1 <pid>              # drop the websocket to exercise reconnect
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

const channelName = "finbot-finances"

// User is the stub "users" table row: Slack user id → finbot identity.
type User struct {
	SlackID  string
	Name     string
	RealName string
	Email    string
	IsBot    bool
}

func main() {
	existingTS := flag.String("ts", "", "reuse an existing TEST message ts instead of posting a new one")
	runFor := flag.Duration("for", 20*time.Minute, "how long to listen before exiting")
	killAfter := flag.Duration("kill-after", 0, "if >0, drop the websocket once after this long (reconnect test)")
	logPath := flag.String("log", "spike.log", "redacted log file (gitignored)")
	flag.Parse()

	_ = godotenv.Load("../../.env")
	appToken, botToken := os.Getenv("SLACK_APP_TOKEN"), os.Getenv("SLACK_BOT_TOKEN")
	if !strings.HasPrefix(appToken, "xapp-") || !strings.HasPrefix(botToken, "xoxb-") {
		log.Fatal("SLACK_APP_TOKEN (xapp-) and SLACK_BOT_TOKEN (xoxb-) must be set")
	}

	f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	red := newRedactor()
	logger := log.New(&redactingWriter{w: io.MultiWriter(os.Stdout, f), r: red}, "", log.LstdFlags|log.Lmicroseconds)

	ctx, cancel := context.WithTimeout(context.Background(), *runFor)
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() { <-sigs; cancel() }()

	killer := &connKiller{}
	api := slack.New(botToken, slack.OptionAppLevelToken(appToken))
	sm := socketmode.New(api, socketmode.OptionDialer(&websocket.Dialer{
		NetDialContext:   killer.dial,
		HandshakeTimeout: 30 * time.Second,
	}))

	s := &spike{api: api, sm: sm, log: logger, red: red, killer: killer, users: map[string]*User{}}
	if err := s.setup(ctx, *existingTS); err != nil {
		logger.Fatalf("setup: %v", err)
	}

	go s.watchKills(ctx, *killAfter)
	go s.handleEvents(ctx)
	if err := sm.RunContext(ctx); err != nil && ctx.Err() == nil {
		logger.Fatalf("socketmode run: %v", err)
	}
	logger.Printf("exiting: %v", ctx.Err())
}

type spike struct {
	api    *slack.Client
	sm     *socketmode.Client
	log    *log.Logger
	red    *redactor
	killer *connKiller

	botUserID string
	channelID string
	msgTS     string

	mu       sync.Mutex
	users    map[string]*User
	killedAt time.Time
}

func (s *spike) setup(ctx context.Context, existingTS string) error {
	auth, err := s.api.AuthTestContext(ctx)
	if err != nil {
		return fmt.Errorf("auth.test: %w", err)
	}
	s.botUserID = auth.UserID
	s.red.add(auth.User, "<BOT_NAME>")
	s.red.add(auth.Team, "<TEAM_NAME>")
	s.log.Printf("auth.test ok: bot user %s, team %s", auth.UserID, auth.TeamID)

	// Resolve the private channel id at runtime (never hard-coded).
	params := &slack.GetConversationsForUserParameters{Types: []string{"private_channel", "public_channel"}, Limit: 200}
	for {
		chans, cursor, err := s.api.GetConversationsForUserContext(ctx, params)
		if err != nil {
			return fmt.Errorf("users.conversations: %w", err)
		}
		for _, c := range chans {
			if c.Name == channelName {
				s.channelID = c.ID
				s.log.Printf("resolved #%s → %s (is_private=%v)", channelName, c.ID, c.IsPrivate)
			}
		}
		if cursor == "" || s.channelID != "" {
			break
		}
		params.Cursor = cursor
	}
	if s.channelID == "" {
		return fmt.Errorf("bot is not a member of #%s", channelName)
	}

	if err := s.loadUsers(ctx); err != nil {
		return err
	}

	if existingTS != "" {
		s.msgTS = existingTS
		s.log.Printf("reusing TEST message ts=%s", s.msgTS)
		return nil
	}
	_, ts, err := s.api.PostMessageContext(ctx, s.channelID,
		slack.MsgOptionText("[SPIKE TEST] categorize TEST: Costco $12.34?", false),
		slack.MsgOptionBlocks(questionBlocks()...))
	if err != nil {
		return fmt.Errorf("chat.postMessage: %w", err)
	}
	s.msgTS = ts
	s.log.Printf("posted TEST question ts=%s", ts)
	return nil
}

func questionBlocks() []slack.Block {
	text := slack.NewTextBlockObject(slack.MarkdownType,
		":test_tube: *[SPIKE TEST — finbot M0-3, please ignore the content]*\n"+
			"Fake transaction *TEST: Costco $12.34* — which category?\n"+
			"_Click any button, then reply in this thread with anything._", false, false)
	btn := func(actionID, label, value string) *slack.ButtonBlockElement {
		return slack.NewButtonBlockElement(actionID, value, slack.NewTextBlockObject(slack.PlainTextType, label, false, false))
	}
	return []slack.Block{
		slack.NewSectionBlock(text, nil, nil),
		slack.NewActionBlock("spike_q",
			btn("cat_pick_1", "TEST Discretionary", "txn:test-1|cat:discretionary"),
			btn("cat_pick_2", "TEST Groceries", "txn:test-1|cat:groceries"),
			btn("cat_pick_3", "TEST Household", "txn:test-1|cat:household"),
			btn("cat_other", "other…", "txn:test-1|cat:other"),
		),
	}
}

// loadUsers maps channel members to the stub users table via users.info.
func (s *spike) loadUsers(ctx context.Context) error {
	members, _, err := s.api.GetUsersInConversationContext(ctx, &slack.GetUsersInConversationParameters{ChannelID: s.channelID})
	if err != nil {
		return fmt.Errorf("conversations.members: %w", err)
	}
	for _, id := range members {
		u, err := s.lookupUser(ctx, id)
		if err != nil {
			return err
		}
		s.log.Printf("users table: %s name=%s real_name=%s email_present=%v is_bot=%v",
			u.SlackID, u.Name, u.RealName, u.Email != "", u.IsBot)
	}
	return nil
}

func (s *spike) lookupUser(ctx context.Context, id string) (*User, error) {
	s.mu.Lock()
	u, ok := s.users[id]
	s.mu.Unlock()
	if ok {
		return u, nil
	}
	info, err := s.api.GetUserInfoContext(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("users.info: %w", err)
	}
	u = &User{SlackID: id, Name: info.Name, RealName: info.RealName, Email: info.Profile.Email, IsBot: info.IsBot}
	for _, n := range []string{info.Name, info.RealName, info.Profile.DisplayName, info.Profile.FirstName, info.Profile.LastName} {
		s.red.add(n, "<NAME>")
	}
	s.mu.Lock()
	s.users[id] = u
	s.mu.Unlock()
	return u, nil
}

func (s *spike) handleEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-s.sm.Events:
			if !ok {
				return
			}
			s.handle(ctx, evt)
		}
	}
}

func (s *spike) handle(ctx context.Context, evt socketmode.Event) {
	switch evt.Type {
	case socketmode.EventTypeConnecting, socketmode.EventTypeConnected, socketmode.EventTypeConnectionError,
		socketmode.EventTypeDisconnect, socketmode.EventTypeIncomingError, socketmode.EventTypeErrorWriteFailed:
		s.log.Printf("conn event: %s %v", evt.Type, evt.Data)
	case socketmode.EventTypeHello:
		s.mu.Lock()
		killed := s.killedAt
		s.killedAt = time.Time{}
		s.mu.Unlock()
		if !killed.IsZero() {
			s.log.Printf("conn event: hello — RECONNECTED %s after socket kill", time.Since(killed).Round(time.Millisecond))
		} else {
			s.log.Printf("conn event: hello")
		}
	case socketmode.EventTypeEventsAPI:
		s.sm.Ack(*evt.Request)
		s.onEventsAPI(ctx, evt)
	case socketmode.EventTypeInteractive:
		s.sm.Ack(*evt.Request)
		s.onInteractive(ctx, evt)
	default:
		if evt.Request != nil {
			s.sm.Ack(*evt.Request)
		}
		s.log.Printf("other event: %s", evt.Type)
	}
}

func (s *spike) onEventsAPI(ctx context.Context, evt socketmode.Event) {
	ea, ok := evt.Data.(slackevents.EventsAPIEvent)
	if !ok {
		return
	}
	msg, ok := ea.InnerEvent.Data.(*slackevents.MessageEvent)
	if !ok {
		s.log.Printf("events_api: inner type %s (ignored)", ea.InnerEvent.Type)
		return
	}
	s.log.Printf("events_api message: channel_type=%s subtype=%q bot_id_set=%v thread_ts=%q latency=%s",
		msg.ChannelType, msg.SubType, msg.BotID != "", msg.ThreadTimeStamp, sinceSlackTS(msg.TimeStamp))
	if msg.ThreadTimeStamp != s.msgTS || msg.BotID != "" || msg.User == "" {
		return
	}
	u, err := s.lookupUser(ctx, msg.User)
	if err != nil {
		s.log.Printf("lookup: %v", err)
		return
	}
	s.log.Printf("THREAD REPLY from %s (%s) text=%q", u.SlackID, u.RealName, msg.Text)
	s.log.Printf("THREAD REPLY envelope payload: %s", evt.Request.Payload)
	_, _, err = s.api.PostMessageContext(ctx, s.channelID, slack.MsgOptionTS(s.msgTS),
		slack.MsgOptionText("[SPIKE TEST] got your threaded reply — thanks!", false))
	if err != nil {
		s.log.Printf("threaded ack: %v", err)
	}
}

func (s *spike) onInteractive(ctx context.Context, evt socketmode.Event) {
	cb, ok := evt.Data.(slack.InteractionCallback)
	if !ok || cb.Type != slack.InteractionTypeBlockActions || len(cb.ActionCallback.BlockActions) == 0 {
		s.log.Printf("interactive: type %s (ignored)", cb.Type)
		return
	}
	a := cb.ActionCallback.BlockActions[0]
	s.log.Printf("BLOCK_ACTION user=%s action_id=%s value=%s block_id=%s message_ts=%s channel=%s latency(action_ts→recv)=%s",
		cb.User.ID, a.ActionID, a.Value, a.BlockID, cb.Container.MessageTs, cb.Channel.ID, sinceSlackTS(string(a.ActionTs)))
	u, err := s.lookupUser(ctx, cb.User.ID) // before logging the payload so names get redacted
	if err != nil {
		s.log.Printf("lookup: %v", err)
		return
	}
	s.log.Printf("BLOCK_ACTION envelope payload: %s", evt.Request.Payload)
	choice := strings.TrimPrefix(a.Value[strings.Index(a.Value, "|")+1:], "cat:")
	text := fmt.Sprintf(":white_check_mark: *[SPIKE TEST]* TEST: Costco $12.34 → *%s* (chosen by %s). Now reply in this thread.", choice, u.RealName)
	_, _, _, err = s.api.UpdateMessageContext(ctx, cb.Channel.ID, cb.Container.MessageTs,
		slack.MsgOptionText(text, false),
		slack.MsgOptionBlocks(slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil)))
	if err != nil {
		s.log.Printf("chat.update: %v", err)
		return
	}
	s.log.Printf("chat.update ok: buttons replaced with choice %q", choice)
}

// watchKills drops the live websocket on SIGUSR1 (and once after killAfter, if set).
func (s *spike) watchKills(ctx context.Context, killAfter time.Duration) {
	usr1 := make(chan os.Signal, 1)
	signal.Notify(usr1, syscall.SIGUSR1)
	var timer <-chan time.Time
	if killAfter > 0 {
		timer = time.After(killAfter)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-usr1:
		case <-timer:
		}
		s.mu.Lock()
		s.killedAt = time.Now()
		s.mu.Unlock()
		s.log.Printf("KILLING websocket TCP connection (reconnect test): closed=%v", s.killer.kill())
	}
}

// connKiller remembers the last dialed TCP conn so it can be closed out from under the websocket.
type connKiller struct {
	mu   sync.Mutex
	conn net.Conn
}

func (k *connKiller) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := (&net.Dialer{Timeout: 30 * time.Second}).DialContext(ctx, network, addr)
	if err == nil {
		k.mu.Lock()
		k.conn = c
		k.mu.Unlock()
	}
	return c, err
}

func (k *connKiller) kill() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.conn == nil {
		return false
	}
	return k.conn.Close() == nil
}

func sinceSlackTS(ts string) time.Duration {
	f, err := strconv.ParseFloat(ts, 64)
	if err != nil {
		return 0
	}
	return time.Since(time.Unix(0, int64(f*1e9))).Round(time.Millisecond)
}

// redactor replaces Slack ids, emails, URLs, secrets and known names with stable placeholders.
type redactor struct {
	mu    sync.Mutex
	ids   map[string]string
	names map[string]string
}

var (
	idRe     = regexp.MustCompile(`\b[UWCGDTBAE][A-Z0-9]{8,12}\b`)
	emailRe  = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	urlRe    = regexp.MustCompile(`https:\\?/\\?/[^"\s]+`)
	secretRe = regexp.MustCompile(`("(?:token|trigger_id|hash|client_msg_id|event_context|app_id|api_app_id)":)"[^"]*"`)
	tokenRe  = regexp.MustCompile(`xox[a-z]-[A-Za-z0-9-]+|xapp-[A-Za-z0-9-]+`)
)

func newRedactor() *redactor {
	return &redactor{ids: map[string]string{}, names: map[string]string{}}
}

func (r *redactor) add(name, placeholder string) {
	if len(strings.TrimSpace(name)) < 3 {
		return
	}
	r.mu.Lock()
	r.names[name] = placeholder
	r.mu.Unlock()
}

func (r *redactor) redact(s string) string {
	s = tokenRe.ReplaceAllString(s, "<TOKEN>")
	s = secretRe.ReplaceAllString(s, `$1"<REDACTED>"`)
	s = urlRe.ReplaceAllString(s, "<URL>")
	s = emailRe.ReplaceAllString(s, "<EMAIL>")
	r.mu.Lock()
	defer r.mu.Unlock()
	s = idRe.ReplaceAllStringFunc(s, func(id string) string {
		if p, ok := r.ids[id]; ok {
			return p
		}
		p := fmt.Sprintf("<%c_ID_%d>", id[0], len(r.ids)+1)
		r.ids[id] = p
		return p
	})
	for n, p := range r.names {
		s = strings.ReplaceAll(s, n, p)
	}
	return s
}

type redactingWriter struct {
	w io.Writer
	r *redactor
}

func (rw *redactingWriter) Write(p []byte) (int, error) {
	if _, err := rw.w.Write([]byte(rw.r.redact(string(p)))); err != nil {
		return 0, err
	}
	return len(p), nil
}
