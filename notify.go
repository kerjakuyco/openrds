package main

// Notifications, like RDS event subscriptions: a message to Slack, Discord,
// Telegram, any webhook, or email when a database or a backup needs
// attention. Each channel chooses the kinds of events it receives. Only the
// standard library is used, and the API never returns a webhook URL, a bot
// token or the SMTP password.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// The kinds of events a channel can receive.
const (
	categoryHealth   = "health"   // a database is unhealthy, crashed, ran out of memory, stopped, or recovered
	categoryFailures = "failures" // an operation such as a backup, restore or copy failed
	categoryDisk     = "disk"     // free space for backups runs low, or recovers
)

var (
	notifyCategories = []string{categoryHealth, categoryFailures, categoryDisk}
	channelKinds     = []string{"slack", "discord", "telegram", "webhook", "email"}
	smtpSecurity     = []string{"starttls", "tls", "none"}

	tokenPattern = regexp.MustCompile(`^[0-9]{3,20}:[A-Za-z0-9_-]{20,100}$`)
	chatPattern  = regexp.MustCompile(`^-?[0-9]{1,20}$|^@[A-Za-z0-9_]{5,32}$`)

	// Variables so tests can use local servers.
	telegramAPI   = "https://api.telegram.org"
	webhookClient = &http.Client{Timeout: 15 * time.Second}
)

// The same failure, such as a copy retried every 10 minutes, is sent again
// only after this long.
const failureRepeat = 6 * time.Hour

type notifyConfig struct {
	SMTP     *smtpConfig     `json:"smtp,omitempty"`
	Channels []notifyChannel `json:"channels"`
}

type smtpConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Security string `json:"security"` // starttls, tls or none
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	From     string `json:"from"`
}

type notifyChannel struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	Name       string   `json:"name"`
	URL        string   `json:"url,omitempty"`    // slack, discord and webhook; a secret
	Token      string   `json:"token,omitempty"`  // telegram bot token; a secret
	ChatID     string   `json:"chatId,omitempty"` // telegram
	To         []string `json:"to,omitempty"`     // email
	Categories []string `json:"categories"`
}

type alert struct {
	Category string    `json:"category"`
	Severity string    `json:"severity"` // critical, warning, resolved or info
	Database string    `json:"database,omitempty"`
	Title    string    `json:"title"`
	Detail   string    `json:"detail,omitempty"`
	At       time.Time `json:"at"`
}

type alertState struct {
	cfgMu    sync.Mutex // one change to the settings file at a time
	watching sync.Mutex // one health check at a time
	pending  sync.WaitGroup
	mu       sync.Mutex
	delivery map[string]delivery   // last message by channel ID
	sent     map[string]time.Time  // failures already sent, to send each once per failureRepeat
	health   map[string]healthMark // by Compose project
	disk     int                   // 0 enough space, 1 low, 2 below the reserve
	server   string
}

type delivery struct {
	At    time.Time
	Error string
}

// What the last health check saw of a database.
type healthMark struct {
	container string
	restarts  int
	problem   string
}

func (m *manager) notifyPath() string { return filepath.Join(m.dir, "notifications.json") }

func (m *manager) loadNotify() (notifyConfig, error) {
	var cfg notifyConfig
	err := readJSON(m.notifyPath(), &cfg)
	if errors.Is(err, os.ErrNotExist) {
		return notifyConfig{}, nil
	}
	return cfg, err
}

// ---------- validation

func cleanLabel(s string, max int) (string, bool) {
	s = strings.TrimSpace(s)
	return s, utf8.RuneCountInString(s) <= max && !strings.ContainsFunc(s, unicode.IsControl)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Settings in canonical form. An empty password keeps the saved one.
func checkSMTP(in smtpConfig, current *smtpConfig) (smtpConfig, error) {
	cfg := in
	cfg.Host = strings.TrimSpace(cfg.Host)
	if !hostPattern.MatchString(cfg.Host) {
		return cfg, fail(400, "Enter the SMTP server’s host name, such as smtp.gmail.com.")
	}
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return cfg, fail(400, "Use a port from 1 to 65535: usually 587 for STARTTLS or 465 for TLS.")
	}
	if cfg.Security == "" {
		cfg.Security = "starttls"
	}
	if !slices.Contains(smtpSecurity, cfg.Security) {
		return cfg, fail(400, "Choose starttls, tls or none.")
	}
	from, err := mail.ParseAddress(strings.TrimSpace(cfg.From))
	if err != nil {
		return cfg, fail(400, "Enter the sender address, such as OpenRDS <alerts@example.com>.")
	}
	cfg.From = from.String()
	var ok bool
	if cfg.Username, ok = cleanLabel(cfg.Username, 256); !ok {
		return cfg, fail(400, "Use a user name of at most 256 characters.")
	}
	if cfg.Password == "" && current != nil {
		cfg.Password = current.Password
	}
	if cfg.Username == "" {
		cfg.Password = ""
	} else if cfg.Password == "" || len(cfg.Password) > 512 || strings.ContainsAny(cfg.Password, "\r\n") {
		return cfg, fail(400, "Enter the password for this user.")
	}
	if cfg.Username != "" && cfg.Security == "none" && !isLoopback(cfg.Host) {
		return cfg, fail(400, "Choose STARTTLS or TLS: OpenRDS does not send a password over an unencrypted connection.")
	}
	return cfg, nil
}

// A channel in canonical form. Its kind is fixed once created, and an empty
// URL or token keeps the saved one.
func checkChannel(in notifyChannel, current *notifyChannel, cfg notifyConfig) (notifyChannel, error) {
	ch := in
	if current != nil {
		ch.ID, ch.Kind = current.ID, current.Kind
	}
	if !slices.Contains(channelKinds, ch.Kind) {
		return ch, fail(400, "Choose slack, discord, telegram, webhook or email.")
	}
	var ok bool
	if ch.Name, ok = cleanLabel(ch.Name, 60); !ok || ch.Name == "" {
		return ch, fail(400, "Name the channel in 1–60 characters, such as #ops-alerts.")
	}
	switch ch.Kind {
	case "slack", "discord", "webhook":
		ch.Token, ch.ChatID, ch.To = "", "", nil
		if ch.URL = strings.TrimSpace(ch.URL); ch.URL == "" && current != nil {
			ch.URL = current.URL
		}
		u, err := url.Parse(ch.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || len(ch.URL) > 2048 {
			return ch, fail(400, "Enter the webhook URL, starting with https://.")
		}
	case "telegram":
		ch.URL, ch.To = "", nil
		if ch.Token = strings.TrimSpace(ch.Token); ch.Token == "" && current != nil {
			ch.Token = current.Token
		}
		if !tokenPattern.MatchString(ch.Token) {
			return ch, fail(400, "Enter the bot token from @BotFather, such as 123456789:AAH…")
		}
		if ch.ChatID = strings.TrimSpace(ch.ChatID); !chatPattern.MatchString(ch.ChatID) {
			return ch, fail(400, "Enter the chat ID, such as -1001234567890, or a public channel such as @ops_alerts.")
		}
	case "email":
		ch.URL, ch.Token, ch.ChatID = "", "", ""
		if cfg.SMTP == nil {
			return ch, fail(409, "Set up the email server (SMTP) first.")
		}
		var to []string
		for _, t := range ch.To {
			if t = strings.TrimSpace(t); t == "" {
				continue
			}
			a, err := mail.ParseAddress(t)
			if err != nil {
				return ch, fail(400, fmt.Sprintf("%q is not an email address.", t))
			}
			if !slices.Contains(to, a.Address) {
				to = append(to, a.Address)
			}
		}
		if len(to) == 0 || len(to) > 20 {
			return ch, fail(400, "Enter 1–20 email addresses.")
		}
		ch.To = to
	}
	var categories []string
	for _, c := range notifyCategories {
		if slices.Contains(ch.Categories, c) {
			categories = append(categories, c)
		}
	}
	for _, c := range ch.Categories {
		if !slices.Contains(notifyCategories, c) {
			return ch, fail(400, "Choose events from health, failures and disk.")
		}
	}
	if len(categories) == 0 {
		return ch, fail(400, "Choose at least one kind of event.")
	}
	ch.Categories = categories
	return ch, nil
}

// ---------- views: what the API returns, without secrets

type channelView struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Name       string     `json:"name"`
	Target     string     `json:"target"` // where messages go, with secrets left out
	ChatID     string     `json:"chatId,omitempty"`
	To         []string   `json:"to,omitempty"`
	Categories []string   `json:"categories"`
	LastSentAt *time.Time `json:"lastSentAt,omitempty"`
	LastError  string     `json:"lastError,omitempty"`
}

type smtpView struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Security    string `json:"security"`
	Username    string `json:"username,omitempty"`
	From        string `json:"from"`
	PasswordSet bool   `json:"passwordSet"`
}

type notifyView struct {
	SMTP     *smtpView     `json:"smtp,omitempty"`
	Channels []channelView `json:"channels"`
}

func hostOf(target string) string {
	if u, err := url.Parse(target); err == nil && u.Host != "" {
		return u.Host
	}
	return "the server"
}

func (ch notifyChannel) target() string {
	switch ch.Kind {
	case "telegram":
		return "Telegram chat " + ch.ChatID
	case "email":
		return strings.Join(ch.To, ", ")
	}
	u, err := url.Parse(ch.URL)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/…" // the path is often the secret
}

func (m *manager) notifyView() (notifyView, error) {
	cfg, err := m.loadNotify()
	if err != nil {
		return notifyView{}, err
	}
	v := notifyView{Channels: []channelView{}}
	if s := cfg.SMTP; s != nil {
		v.SMTP = &smtpView{s.Host, s.Port, s.Security, s.Username, s.From, s.Password != ""}
	}
	m.alerts.mu.Lock()
	defer m.alerts.mu.Unlock()
	for _, ch := range cfg.Channels {
		cv := channelView{ID: ch.ID, Kind: ch.Kind, Name: ch.Name, Target: ch.target(), ChatID: ch.ChatID, To: ch.To, Categories: ch.Categories}
		if d, ok := m.alerts.delivery[ch.ID]; ok {
			at := d.At
			cv.LastSentAt, cv.LastError = &at, d.Error
		}
		v.Channels = append(v.Channels, cv)
	}
	return v, nil
}

// ---------- changes

// Apply a change to the settings file while holding its lock.
func (m *manager) changeNotify(change func(*notifyConfig) error) error {
	m.alerts.cfgMu.Lock()
	defer m.alerts.cfgMu.Unlock()
	cfg, err := m.loadNotify()
	if err != nil {
		return err
	}
	if err := change(&cfg); err != nil {
		return err
	}
	return writeJSON(m.notifyPath(), cfg)
}

func (m *manager) noteNotify(ctx context.Context, action, detail string) {
	m.logEvent(ctx, event{Name: "Notifications", Kind: "notifications", Action: action, Result: "completed", Detail: detail})
}

func testAlert(ch notifyChannel) alert {
	return alert{Category: "test", Severity: "info", Title: "Test message from OpenRDS",
		Detail: "Alerts about " + strings.Join(ch.Categories, ", ") + " will arrive here.", At: time.Now().UTC()}
}

// Send a test message, then save: a mistyped URL or address is caught at once.
func (m *manager) addChannel(ctx context.Context, in notifyChannel) (notifyChannel, error) {
	cfg, err := m.loadNotify()
	if err != nil {
		return in, err
	}
	ch, err := checkChannel(in, nil, cfg)
	if err != nil {
		return ch, err
	}
	if len(cfg.Channels) >= 50 {
		return ch, fail(409, "Use at most 50 channels.")
	}
	if err := m.trySend(ctx, cfg, ch); err != nil {
		return ch, err
	}
	ch.ID = randomHex(8) // only now, so a failed test leaves no delivery record behind
	err = m.changeNotify(func(c *notifyConfig) error {
		c.Channels = append(c.Channels, ch)
		return nil
	})
	if err == nil {
		m.recordDelivery(ch.ID, nil)
		m.noteNotify(ctx, "Add notification channel", ch.Name+" ("+ch.Kind+")")
	}
	return ch, err
}

func (m *manager) updateChannel(ctx context.Context, id string, in notifyChannel) (notifyChannel, error) {
	cfg, err := m.loadNotify()
	if err != nil {
		return in, err
	}
	i := slices.IndexFunc(cfg.Channels, func(c notifyChannel) bool { return c.ID == id })
	if i < 0 {
		return in, fail(404, "Notification channel not found.")
	}
	current := cfg.Channels[i]
	ch, err := checkChannel(in, &current, cfg)
	if err != nil {
		return ch, err
	}
	if ch.URL != current.URL || ch.Token != current.Token || ch.ChatID != current.ChatID || !slices.Equal(ch.To, current.To) {
		if err := m.trySend(ctx, cfg, ch); err != nil {
			return ch, err
		}
	}
	err = m.changeNotify(func(c *notifyConfig) error {
		j := slices.IndexFunc(c.Channels, func(c notifyChannel) bool { return c.ID == id })
		if j < 0 {
			return fail(404, "Notification channel not found.")
		}
		c.Channels[j] = ch
		return nil
	})
	if err == nil {
		m.noteNotify(ctx, "Change notification channel", ch.Name+" ("+ch.Kind+")")
	}
	return ch, err
}

func (m *manager) deleteChannel(ctx context.Context, id string) error {
	var removed notifyChannel
	err := m.changeNotify(func(c *notifyConfig) error {
		i := slices.IndexFunc(c.Channels, func(c notifyChannel) bool { return c.ID == id })
		if i < 0 {
			return fail(404, "Notification channel not found.")
		}
		removed = c.Channels[i]
		c.Channels = slices.Delete(c.Channels, i, i+1)
		return nil
	})
	if err == nil {
		m.alerts.mu.Lock()
		delete(m.alerts.delivery, id)
		m.alerts.mu.Unlock()
		m.noteNotify(ctx, "Delete notification channel", removed.Name+" ("+removed.Kind+")")
	}
	return err
}

func (m *manager) testChannel(ctx context.Context, id string) error {
	cfg, err := m.loadNotify()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(cfg.Channels, func(c notifyChannel) bool { return c.ID == id })
	if i < 0 {
		return fail(404, "Notification channel not found.")
	}
	return m.trySend(ctx, cfg, cfg.Channels[i])
}

func (m *manager) trySend(ctx context.Context, cfg notifyConfig, ch notifyChannel) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := m.deliver(ctx, cfg, ch, testAlert(ch))
	if ch.ID != "" {
		m.recordDelivery(ch.ID, err)
	}
	if err != nil {
		return fail(502, "The test message did not arrive: "+err.Error())
	}
	return nil
}

// Check the server answers and accepts the sign-in before saving.
func (m *manager) saveSMTP(ctx context.Context, in smtpConfig) error {
	cfg, err := m.loadNotify()
	if err != nil {
		return err
	}
	s, err := checkSMTP(in, cfg.SMTP)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, err := dialSMTP(ctx, s)
	if err != nil {
		return fail(502, "Could not use the email server: "+err.Error())
	}
	c.Quit()
	err = m.changeNotify(func(c *notifyConfig) error {
		c.SMTP = &s
		return nil
	})
	if err == nil {
		m.noteNotify(ctx, "Set up email server", s.Host+":"+strconv.Itoa(s.Port))
	}
	return err
}

func (m *manager) removeSMTP(ctx context.Context) error {
	err := m.changeNotify(func(c *notifyConfig) error {
		if slices.ContainsFunc(c.Channels, func(ch notifyChannel) bool { return ch.Kind == "email" }) {
			return fail(409, "Delete the email channels first.")
		}
		c.SMTP = nil
		return nil
	})
	if err == nil {
		m.noteNotify(ctx, "Remove email server", "")
	}
	return err
}

// ---------- delivery

func (m *manager) recordDelivery(id string, err error) {
	d := delivery{At: time.Now().UTC()}
	if err != nil {
		d.Error = err.Error()
		log.Printf("notification channel %s: %v", id, err)
	}
	m.alerts.mu.Lock()
	if m.alerts.delivery == nil {
		m.alerts.delivery = make(map[string]delivery)
	}
	m.alerts.delivery[id] = d
	m.alerts.mu.Unlock()
}

// The name messages carry, so a team with several servers knows which one.
func (m *manager) serverName(ctx context.Context) string {
	m.alerts.mu.Lock()
	name := m.alerts.server
	m.alerts.mu.Unlock()
	if name != "" {
		return name
	}
	var out bytes.Buffer
	if m.docker.run(ctx, nil, nil, &out, "info", "--format", "{{.Name}}") == nil {
		name = strings.TrimSpace(out.String())
	}
	if name == "" {
		name, _ = os.Hostname()
	}
	if name == "" {
		return "OpenRDS"
	}
	m.alerts.mu.Lock()
	m.alerts.server = name
	m.alerts.mu.Unlock()
	return name
}

var severityMark = map[string]string{"critical": "🔴", "warning": "🟠", "resolved": "✅", "info": "ℹ️"}

func (a alert) text(server string) string {
	var b strings.Builder
	b.WriteString(severityMark[a.Severity] + " " + a.Title)
	if a.Detail != "" {
		b.WriteString("\n" + a.Detail)
	}
	fmt.Fprintf(&b, "\n%s · %s", server, a.At.UTC().Format("2006-01-02 15:04 UTC"))
	if a.Database != "" {
		fmt.Fprintf(&b, "\nhttp://127.0.0.1:%d/#/databases/%s", consolePort(), a.Database)
	}
	text := b.String()
	if r := []rune(text); len(r) > 1900 { // Discord allows 2,000 characters
		text = string(r[:1900]) + "…"
	}
	return text
}

func (a alert) subject(server string) string {
	prefix := ""
	if a.Severity == "resolved" {
		prefix = "Resolved: "
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, "[OpenRDS "+server+"] "+prefix+a.Title)
}

// Slack reads &, < and > as markup.
var slackEscape = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

type webhookPayload struct {
	Source string `json:"source"`
	Server string `json:"server"`
	alert
	Text string `json:"text"` // the whole message, for receivers that only show text
}

func (m *manager) deliver(ctx context.Context, cfg notifyConfig, ch notifyChannel, a alert) error {
	server := m.serverName(ctx)
	text := a.text(server)
	switch ch.Kind {
	case "slack":
		return postJSON(ctx, ch.URL, map[string]string{"text": slackEscape.Replace(text)})
	case "discord":
		return postJSON(ctx, ch.URL, map[string]any{"content": text, "allowed_mentions": map[string]any{"parse": []string{}}})
	case "telegram":
		return postJSON(ctx, telegramAPI+"/bot"+ch.Token+"/sendMessage", map[string]any{"chat_id": ch.ChatID, "text": text, "disable_web_page_preview": true})
	case "webhook":
		return postJSON(ctx, ch.URL, webhookPayload{"openrds", server, a, text})
	case "email":
		if cfg.SMTP == nil {
			return errors.New("the email server is not set up")
		}
		return sendMail(ctx, *cfg.SMTP, ch.To, a.subject(server), text+"\n\nSent by OpenRDS.")
	}
	return fmt.Errorf("unknown channel kind %q", ch.Kind)
}

func postJSON(ctx context.Context, target string, body any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(data))
	if err != nil {
		return errors.New("the address is not valid")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "OpenRDS")
	resp, err := webhookClient.Do(req)
	if err != nil {
		var u *url.Error
		if errors.As(err, &u) {
			err = u.Err // its message repeats the URL, which holds the secret
		}
		return fmt.Errorf("could not reach %s: %v", hostOf(target), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("%s answered %s %s", hostOf(target), resp.Status, strings.TrimSpace(string(msg)))
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return nil
}

func dialSMTP(ctx context.Context, cfg smtpConfig) (*smtp.Client, error) {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	d := &net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	var err error
	if cfg.Security == "tls" {
		conn, err = (&tls.Dialer{NetDialer: d, Config: &tls.Config{ServerName: cfg.Host}}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("could not reach %s: %v", addr, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetDeadline(deadline)
	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("%s did not answer as an email server: %v", addr, err)
	}
	if cfg.Security == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			c.Close()
			return nil, errors.New("the server does not offer STARTTLS; choose TLS (usually port 465) or none")
		}
		if err := c.StartTLS(&tls.Config{ServerName: cfg.Host}); err != nil {
			c.Close()
			return nil, fmt.Errorf("STARTTLS failed: %v", err)
		}
	}
	if cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)); err != nil {
			c.Close()
			return nil, fmt.Errorf("sign-in failed: %v", err)
		}
	}
	return c, nil
}

func sendMail(ctx context.Context, cfg smtpConfig, to []string, subject, body string) error {
	c, err := dialSMTP(ctx, cfg)
	if err != nil {
		return err
	}
	defer c.Close()
	from, err := mail.ParseAddress(cfg.From)
	if err != nil {
		return err
	}
	if err := c.Mail(from.Address); err != nil {
		return fmt.Errorf("the server refused the sender %s: %v", from.Address, err)
	}
	for _, t := range to {
		if err := c.Rcpt(t); err != nil {
			return fmt.Errorf("the server refused %s: %v", t, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(mailMessage(from.String(), to, subject, body)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("the server did not accept the message: %v", err)
	}
	return c.Quit()
}

func mailMessage(from string, to []string, subject, body string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@openrds>\r\n", from, strings.Join(to, ", "),
		mime.QEncoding.Encode("utf-8", subject), time.Now().Format(time.RFC1123Z), randomHex(16))
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
	w := quotedprintable.NewWriter(&b)
	w.Write([]byte(strings.ReplaceAll(body, "\n", "\r\n")))
	w.Close()
	return b.Bytes()
}

// Send an alert to every channel that receives its category, in the
// background: a slow or failing channel never holds up the others.
func (m *manager) notify(a alert) {
	cfg, err := m.loadNotify()
	if err != nil {
		log.Printf("reading notification settings: %v", err)
		return
	}
	if a.At.IsZero() {
		a.At = time.Now().UTC()
	}
	for _, ch := range cfg.Channels {
		if !slices.Contains(ch.Categories, a.Category) {
			continue
		}
		m.alerts.pending.Add(1)
		go func() {
			defer m.alerts.pending.Done()
			ctx, cancel := context.WithTimeout(m.backgroundContext(), 30*time.Second)
			defer cancel()
			m.recordDelivery(ch.ID, m.deliver(ctx, cfg, ch, a))
		}()
	}
}

// Record a problem the monitor found in Events, and send it.
func (m *manager) raise(ctx context.Context, e event, a alert) {
	e.alerted = true
	m.logEvent(ctx, e)
	m.notify(a)
}

// A failed operation, sent once per failureRepeat: an S3 copy that keeps
// failing is retried every 10 minutes, and one message is enough.
func (m *manager) alertFailure(e event) {
	key := e.Name + "\x00" + e.Action + "\x00" + e.Detail
	m.alerts.mu.Lock()
	if m.alerts.sent == nil {
		m.alerts.sent = make(map[string]time.Time)
	}
	if last, ok := m.alerts.sent[key]; ok && e.At.Sub(last) < failureRepeat {
		m.alerts.mu.Unlock()
		return
	}
	maps.DeleteFunc(m.alerts.sent, func(_ string, at time.Time) bool { return e.At.Sub(at) >= failureRepeat })
	m.alerts.sent[key] = e.At
	m.alerts.mu.Unlock()
	a := alert{Category: categoryFailures, Severity: "warning", Title: e.Action + " failed: " + e.Name, Detail: e.Detail, At: e.At}
	if e.Kind == "" {
		a.Database = e.Name
	}
	if e.Actor != "" && e.Actor != "system" {
		a.Detail = strings.TrimSpace(a.Detail + " Started by " + e.Actor + ".")
	}
	m.notify(a)
}

// ---------- monitoring

type containerHealth struct {
	id, state, status string
	restarts          int
	oom               bool
	exit              int
}

func (c containerHealth) problem() string {
	switch {
	case c.state == "running" && strings.Contains(c.status, "(unhealthy)"):
		return "unhealthy"
	case c.state == "restarting":
		return "crashing"
	case (c.state == "exited" || c.state == "dead") && c.exit != 0:
		return "stopped"
	}
	return ""
}

func (c containerHealth) healthy() bool {
	return c.state == "running" && strings.Contains(c.status, "(healthy)")
}

// Every database container on every reachable server by Compose project,
// with its restart count.
func (m *manager) inspectDatabases(ctx context.Context) (map[string]containerHealth, error) {
	found := make(map[string]containerHealth)
	for _, s := range m.serverList() {
		if s.ID != localServer && m.recentlyDown(s.ID) {
			continue
		}
		on, err := m.inspectServer(ctx, m.dockerFor(s.ID))
		if err != nil {
			if s.ID == localServer {
				return nil, err
			}
			continue
		}
		maps.Copy(found, on)
	}
	return found, nil
}

func (m *manager) inspectServer(ctx context.Context, docker dockerRunner) (map[string]containerHealth, error) {
	var out bytes.Buffer
	if err := docker.run(ctx, nil, nil, &out, "ps", "--all", "--no-trunc", "--filter", "label=io.openrds.database=true",
		"--format", `{{.ID}}|{{.Label "com.docker.compose.project"}}|{{.State}}|{{.Status}}`); err != nil {
		return nil, err
	}
	found := make(map[string]containerHealth)
	var ids []string
	for _, line := range strings.Split(out.String(), "\n") {
		f := strings.SplitN(strings.TrimSpace(line), "|", 4)
		if len(f) != 4 || f[0] == "" || f[1] == "" {
			continue
		}
		found[f[1]] = containerHealth{id: f[0], state: f[2], status: f[3]}
		ids = append(ids, f[0])
	}
	if len(ids) == 0 {
		return found, nil
	}
	out.Reset()
	args := append([]string{"inspect", "--format", "{{.Id}}|{{.RestartCount}}|{{.State.OOMKilled}}|{{.State.ExitCode}}"}, ids...)
	if err := docker.run(ctx, nil, nil, &out, args...); err != nil {
		return nil, err // a container removed meanwhile; the next check sees the rest
	}
	details := make(map[string][]string)
	for _, line := range strings.Split(out.String(), "\n") {
		if f := strings.Split(strings.TrimSpace(line), "|"); len(f) == 4 {
			details[f[0]] = f
		}
	}
	for project, c := range found {
		if f, ok := details[c.id]; ok {
			c.restarts, _ = strconv.Atoi(f[1])
			c.oom = f[2] == "true"
			c.exit, _ = strconv.Atoi(f[3])
			found[project] = c
		}
	}
	return found, nil
}

// Compare every database with the last check and alert on changes: it turned
// unhealthy, crashed and restarted (for example out of memory), keeps
// crashing, stopped on its own, or is healthy again. Changes an operation
// makes, such as a reboot, are not alerts, and the first check after the
// console starts only takes note.
func (m *manager) watchHealth(ctx context.Context) {
	if !m.alerts.watching.TryLock() {
		return
	}
	defer m.alerts.watching.Unlock()
	records, err := m.records()
	if err != nil {
		return
	}
	found, err := m.inspectDatabases(ctx)
	if err != nil {
		log.Printf("health check: %v", err)
		return
	}
	m.mu.Lock()
	busy := maps.Clone(m.busy)
	m.mu.Unlock()
	type finding struct {
		e event
		a alert
	}
	var findings []finding
	raise := func(r record, severity, title, action, result, detail string) {
		findings = append(findings, finding{
			event{Name: r.Name, Action: action, Result: result, Detail: detail},
			alert{Category: categoryHealth, Severity: severity, Database: r.Name, Title: title, Detail: detail},
		})
	}
	m.alerts.mu.Lock()
	if m.alerts.health == nil {
		m.alerts.health = make(map[string]healthMark)
	}
	current := make(map[string]bool)
	for _, r := range records {
		current[r.Project] = true
		c, ok := found[r.Project]
		if !ok {
			continue // not created yet
		}
		mark, seen := m.alerts.health[r.Project]
		next := healthMark{container: c.id, restarts: c.restarts, problem: mark.problem}
		if !seen || r.PendingRestore {
			next.problem = c.problem()
			m.alerts.health[r.Project] = next
			continue
		}
		if busy[r.Name] {
			m.alerts.health[r.Project] = next // an operation is changing it on purpose
			continue
		}
		problem := c.problem()
		switch {
		case c.id == mark.container && c.restarts > mark.restarts:
			detail := "The server stopped and Docker restarted it. Its logs show why."
			title, action := r.Name+" restarted after a crash", "Restarted after a crash"
			if c.oom {
				title, action = r.Name+" ran out of memory and restarted", "Ran out of memory"
				detail = "It used all of its memory, so the kernel stopped it and Docker restarted it. Raise its memory limit, or lower settings that use memory, such as max_connections."
				if r.MemoryMiB > 0 {
					detail = fmt.Sprintf("It used all of its %d MiB memory limit, so the kernel stopped it and Docker restarted it. Raise the memory limit, or lower settings that use memory, such as max_connections.", r.MemoryMiB)
				}
			}
			if c.healthy() {
				detail += " It is running again."
				next.problem = ""
			} else {
				next.problem = "crashed"
				if problem != "" {
					next.problem = problem
				}
			}
			raise(r, "critical", title, action, "failed", detail)
		case problem != "" && problem != mark.problem:
			next.problem = problem
			switch problem {
			case "unhealthy":
				raise(r, "critical", r.Name+" is unhealthy", "Health check failing", "failed",
					"Its health check keeps failing, so it may not accept connections. Its logs show why.")
			case "crashing":
				raise(r, "critical", r.Name+" keeps crashing", "Crashing on start", "failed",
					"The server exits right after starting and Docker keeps restarting it. Its logs show why, often a setting it refuses.")
			case "stopped":
				detail := fmt.Sprintf("The server exited with code %d. Start it again from the console; its logs show why.", c.exit)
				if c.exit == 137 {
					detail += " Code 137 means it was killed, often for running out of memory."
				}
				raise(r, "critical", r.Name+" stopped unexpectedly", "Stopped unexpectedly", "failed", detail)
			}
		case problem == "" && mark.problem != "" && c.healthy():
			next.problem = ""
			raise(r, "resolved", r.Name+" is healthy again", "Healthy again", "completed", "")
		}
		m.alerts.health[r.Project] = next
	}
	maps.DeleteFunc(m.alerts.health, func(project string, _ healthMark) bool { return !current[project] })
	m.alerts.mu.Unlock()
	for _, f := range findings {
		m.raise(ctx, f.e, f.a)
	}
}

// Alert when free space for backups gets low, falls below the reserve, or
// recovers. A low disk is reported once after the console starts.
func (m *manager) watchDisk(ctx context.Context) {
	free, total, ok := m.space(m.backupDir())
	if !ok {
		return
	}
	reserve := backupReserve(total)
	level := 0
	if free < 2*reserve {
		level = 1
	}
	if free < reserve {
		level = 2
	}
	m.alerts.mu.Lock()
	previous := m.alerts.disk
	m.alerts.disk = level
	m.alerts.mu.Unlock()
	e := event{Name: "Backup disk", Kind: "disk", Result: "failed"}
	a := alert{Category: categoryDisk}
	switch {
	case level > previous && level == 2:
		e.Action, a.Severity, a.Title = "Backups paused: disk almost full", "critical", "Backups are paused: the disk is almost full"
		e.Detail = fmt.Sprintf("%s free. Backups need %s free so databases keep running. Delete old snapshots or add disk space.", sizeLabel(free), sizeLabel(reserve))
	case level > previous:
		e.Action, a.Severity, a.Title = "Disk space low", "warning", "Disk space is running low"
		e.Detail = fmt.Sprintf("%s free. Backups stop below %s so databases keep running.", sizeLabel(free), sizeLabel(reserve))
	case level == 0 && previous > 0:
		e.Action, e.Result, a.Severity, a.Title = "Disk space recovered", "completed", "resolved", "Disk space is back to normal"
		e.Detail = sizeLabel(free) + " free."
	default:
		return
	}
	a.Detail = e.Detail
	m.raise(ctx, e, a)
}

// ---------- API

func notifyRoutes(add func(string, int, endpoint), m *manager) {
	add("GET /api/notifications", roleAdmin, func(w http.ResponseWriter, r *http.Request) error {
		v, err := m.notifyView()
		if err == nil {
			sendJSON(w, 200, v)
		}
		return err
	})
	add("PUT /api/notifications/smtp", roleAdmin, func(w http.ResponseWriter, r *http.Request) error {
		var in smtpConfig
		if err := decode(w, r, &in); err != nil {
			return err
		}
		if err := m.saveSMTP(r.Context(), in); err != nil {
			return err
		}
		v, err := m.notifyView()
		if err == nil {
			sendJSON(w, 200, v)
		}
		return err
	})
	add("DELETE /api/notifications/smtp", roleAdmin, func(w http.ResponseWriter, r *http.Request) error {
		if err := m.removeSMTP(r.Context()); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})
	add("POST /api/notifications/channels", roleAdmin, func(w http.ResponseWriter, r *http.Request) error {
		var in notifyChannel
		if err := decode(w, r, &in); err != nil {
			return err
		}
		ch, err := m.addChannel(r.Context(), in)
		if err == nil {
			sendJSON(w, 201, map[string]string{"id": ch.ID})
		}
		return err
	})
	add("PATCH /api/notifications/channels/{id}", roleAdmin, func(w http.ResponseWriter, r *http.Request) error {
		var in notifyChannel
		if err := decode(w, r, &in); err != nil {
			return err
		}
		if _, err := m.updateChannel(r.Context(), r.PathValue("id"), in); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})
	add("DELETE /api/notifications/channels/{id}", roleAdmin, func(w http.ResponseWriter, r *http.Request) error {
		if err := m.deleteChannel(r.Context(), r.PathValue("id")); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})
	add("POST /api/notifications/channels/{id}/test", roleAdmin, func(w http.ResponseWriter, r *http.Request) error {
		if err := m.testChannel(r.Context(), r.PathValue("id")); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})
}
