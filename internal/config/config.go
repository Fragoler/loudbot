// Package config loads service settings from a TOML file and secrets from the environment.
// Secrets never come from the file: the TOML fields for them are explicitly skipped.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	// The zone database goes into the binary: the runtime image ships no tzdata,
	// and service.timezone must resolve there.
	_ "time/tzdata"

	"github.com/pelletier/go-toml/v2"
)

const (
	// EnvConfigPath overrides the default config file location.
	EnvConfigPath = "CONFIG_PATH"
	// EnvBotToken holds the Telegram bot token.
	EnvBotToken = "BOT_TOKEN"
	// EnvPostgresPassword holds the Postgres password.
	//nolint:gosec // G101: this is the name of an environment variable, not a credential.
	EnvPostgresPassword = "POSTGRES_PASSWORD"

	defaultPath = "config.toml"
)

// Mode is the way the bot receives updates from Telegram.
type Mode string

const (
	ModePolling Mode = "polling"
	ModeWebhook Mode = "webhook"
)

type Config struct {
	Service    Service      `toml:"service"`
	Telegram   Telegram     `toml:"telegram"`
	Postgres   Postgres     `toml:"postgres"`
	Moderation Moderation   `toml:"moderation"`
	Comments   Comments     `toml:"comments"`
	Posts      PostSettings `toml:"posts"`
	Messages   Messages     `toml:"messages"`
}

type Service struct {
	Name     string `toml:"name"`
	LogLevel string `toml:"log_level"`
	// Timezone is the zone achievement rules read the clock in, e.g. the hour
	// window of a "wrote at night" rule. Empty means UTC.
	Timezone string `toml:"timezone"`
}

// Location resolves Timezone. The zone database is embedded (see the blank
// time/tzdata import below), so this works in a container without tzdata.
func (s Service) Location() (*time.Location, error) {
	name := strings.TrimSpace(s.Timezone)
	if name == "" {
		return time.UTC, nil
	}

	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("service.timezone=%q: %w", s.Timezone, err)
	}

	return loc, nil
}

type Telegram struct {
	Mode        Mode   `toml:"mode"`
	BotUsername string `toml:"bot_username"`
	// ChannelUsername builds public links to posts; empty falls back to the /c/
	// form, which only members of a private channel can follow.
	ChannelUsername string `toml:"channel_username"`
	Debug           bool   `toml:"debug"`

	// ChannelID is the channel the bot posts to and watches.
	ChannelID int64 `toml:"channel_id"`
	// DiscussionChatID is the linked discussion group where comments are published.
	DiscussionChatID int64 `toml:"discussion_chat_id"`

	Token string `toml:"-"`
}

type Postgres struct {
	Host     string `toml:"host"`
	Port     int    `toml:"port"`
	User     string `toml:"user"`
	DBName   string `toml:"dbname"`
	SSLMode  string `toml:"sslmode"`
	MaxConns int32  `toml:"max_conns"`

	Password string `toml:"-"`
}

type Moderation struct {
	ChatID int64 `toml:"chat_id"`
}

type Comments struct {
	// MaxTextLen caps the comment body before the nickname prefix is added.
	MaxTextLen int `toml:"max_text_len"`
	// DraftTTL bounds how long a deep-link tap stays valid, e.g. "1h" or "30m".
	DraftTTL       string `toml:"draft_ttl"`
	AnswerLinkText string `toml:"answer_link_text"`
}

// PostSettings are the suggestion flow's tunables. Named apart from the Posts
// copy under Messages, which holds wording rather than behaviour.
type PostSettings struct {
	// MaxTextLen caps the body of a suggested post.
	MaxTextLen int `toml:"max_text_len"`
	// PendingLimit caps how many suggestions one account may have awaiting a
	// decision, so one person cannot bury the moderators. Zero means no limit.
	PendingLimit int `toml:"pending_limit"`
}

// TTL parses DraftTTL; Validate reports a malformed value separately.
func (c Comments) TTL() (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(c.DraftTTL))
	if err != nil {
		return 0, fmt.Errorf("comments.draft_ttl=%q: %w", c.DraftTTL, err)
	}

	if d <= 0 {
		return 0, fmt.Errorf("comments.draft_ttl=%q must be positive", c.DraftTTL)
	}

	return d, nil
}

// Load reads the TOML file at path (falling back to CONFIG_PATH, then config.toml),
// applies defaults, overlays secrets from the environment and validates the result.
func Load(path string) (Config, error) {
	if path == "" {
		path = strings.TrimSpace(os.Getenv(EnvConfigPath))
	}
	if path == "" {
		path = defaultPath
	}

	// G703: the path comes from the operator's own -config flag or CONFIG_PATH,
	// never from a user of the bot, so there is no untrusted input to traverse with.
	raw, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}

	cfg := defaults()
	if err := toml.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}

	applySecrets(&cfg)

	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("config %q: %w", path, err)
	}

	return cfg, nil
}

func defaults() Config {
	return Config{
		Service: Service{
			Name:     "loudbot",
			LogLevel: "info",
		},
		Telegram: Telegram{
			Mode: ModePolling,
		},
		Postgres: Postgres{
			Host:     "localhost",
			Port:     5432,
			SSLMode:  "disable",
			MaxConns: 10,
		},
		Messages: DefaultMessages(),
		Comments: Comments{
			MaxTextLen: 3500,
			DraftTTL:   "1h",
		},
		Posts: PostSettings{
			MaxTextLen:   3500,
			PendingLimit: 3,
		},
	}
}

func applySecrets(cfg *Config) {
	cfg.Telegram.Token = strings.TrimSpace(os.Getenv(EnvBotToken))
	cfg.Postgres.Password = os.Getenv(EnvPostgresPassword)
}

func (c Config) Validate() error {
	var errs []error

	if c.Telegram.Token == "" {
		errs = append(errs, fmt.Errorf("%s is not set", EnvBotToken))
	}

	if strings.TrimSpace(c.Telegram.BotUsername) == "" {
		errs = append(errs, errors.New("telegram.bot_username is required for comment deep links"))
	}

	switch c.Telegram.Mode {
	case ModePolling, ModeWebhook:
	default:
		errs = append(errs, fmt.Errorf("telegram.mode=%q: want %q or %q", c.Telegram.Mode, ModePolling, ModeWebhook))
	}

	if c.Telegram.ChannelID == 0 {
		errs = append(errs, errors.New("telegram.channel_id is required"))
	}

	if c.Telegram.DiscussionChatID == 0 {
		errs = append(errs, errors.New("telegram.discussion_chat_id is required"))
	}

	if c.Moderation.ChatID == 0 {
		errs = append(errs, errors.New("moderation.chat_id is required"))
	}

	if strings.TrimSpace(c.Postgres.Host) == "" {
		errs = append(errs, errors.New("postgres.host is required"))
	}

	if strings.TrimSpace(c.Postgres.DBName) == "" {
		errs = append(errs, errors.New("postgres.dbname is required"))
	}

	if strings.TrimSpace(c.Postgres.User) == "" {
		errs = append(errs, errors.New("postgres.user is required"))
	}

	if c.Postgres.Port <= 0 || c.Postgres.Port > 65535 {
		errs = append(errs, fmt.Errorf("postgres.port=%d is out of range", c.Postgres.Port))
	}

	if c.Comments.MaxTextLen <= 0 {
		errs = append(errs, fmt.Errorf("comments.max_text_len=%d must be positive", c.Comments.MaxTextLen))
	}

	if _, err := c.Comments.TTL(); err != nil {
		errs = append(errs, err)
	}

	if c.Posts.MaxTextLen <= 0 {
		errs = append(errs, fmt.Errorf("posts.max_text_len=%d must be positive", c.Posts.MaxTextLen))
	}

	if c.Posts.PendingLimit < 0 {
		errs = append(errs, fmt.Errorf("posts.pending_limit=%d cannot be negative", c.Posts.PendingLimit))
	}

	errs = append(errs, c.Messages.validate()...)

	if _, err := parseLevel(c.Service.LogLevel); err != nil {
		errs = append(errs, err)
	}

	if _, err := c.Service.Location(); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// LogLevel maps service.log_level onto slog; an unparsable value falls back to info
// so that logging never blocks startup (Validate reports it separately).
func (c Config) LogLevel() slog.Level {
	level, err := parseLevel(c.Service.LogLevel)
	if err != nil {
		return slog.LevelInfo
	}

	return level
}

func parseLevel(raw string) (slog.Level, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.TrimSpace(raw))); err != nil {
		return 0, fmt.Errorf("service.log_level=%q: %w", raw, err)
	}

	return level, nil
}

// DSN builds a connection string for both pgxpool and database/sql. Pool sizing is
// applied in code rather than here, because database/sql rejects pool-only options.
func (p Postgres) DSN() string {
	u := url.URL{
		Scheme: "postgres",
		Host:   net.JoinHostPort(p.Host, strconv.Itoa(p.Port)),
		Path:   "/" + p.DBName,
	}

	if p.Password == "" {
		u.User = url.User(p.User)
	} else {
		u.User = url.UserPassword(p.User, p.Password)
	}

	q := url.Values{}
	if p.SSLMode != "" {
		q.Set("sslmode", p.SSLMode)
	}
	u.RawQuery = q.Encode()

	return u.String()
}
