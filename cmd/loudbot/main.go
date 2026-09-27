package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"loudbot/internal/achievement"
	"loudbot/internal/comment"
	"loudbot/internal/config"
	"loudbot/internal/profile"
	"loudbot/internal/storage"
	"loudbot/internal/suggestion"
	"loudbot/internal/telegram"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	configPath := flag.String("config", "", "path to config.toml (default: $CONFIG_PATH, then ./config.toml)")
	flag.Parse()

	// Bootstrap logger: replaced once the config tells us the real level.
	slog.SetDefault(newLogger(slog.LevelInfo))

	if err := run(*configPath); err != nil {
		slog.Error("bot stopped with error", slog.Any("error", err))
		os.Exit(1)
	}
}

func run(configPath string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	log := newLogger(cfg.LogLevel()).With(
		slog.String("service", cfg.Service.Name),
		slog.String("version", version),
	)
	slog.SetDefault(log)

	if err := storage.Migrate(ctx, cfg.Postgres, log); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	store, err := storage.Open(ctx, cfg.Postgres, log)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	defer store.Close()

	bot, err := telegram.New(cfg, store, log)
	if err != nil {
		return fmt.Errorf("telegram: %w", err)
	}

	ttl, err := cfg.Comments.TTL()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	// Antiabuse is not wired yet, so the core runs with a permissive guard.
	comments := comment.New(store, bot.Publisher(), comment.AllowAll{}, comment.Options{
		BotUsername:   cfg.Telegram.BotUsername,
		ReplyLinkText: cfg.Messages.ReplyLink,
		ChannelID:     cfg.Telegram.ChannelID,
		MaxTextLen:    cfg.Comments.MaxTextLen,
		DraftTTL:      ttl,
	})
	bot.UseComments(comments)
	bot.UseProfiles(profile.New(store))

	location, err := cfg.Service.Location()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	bot.UseAwarder(achievement.New(store, location, log))
	bot.UseSuggestions(suggestion.New(store, suggestion.Options{
		MaxTextLen:   cfg.Posts.MaxTextLen,
		PendingLimit: cfg.Posts.PendingLimit,
	}))

	return bot.Run(ctx)
}

func newLogger(level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}
