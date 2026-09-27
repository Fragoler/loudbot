// Package suggestion is the post-suggestion flow: a reader offers a post to the
// bot in private, moderators decide in their own chat, and an approved post is
// published to the channel by the bot.
//
// The rules about who may do what to a suggestion live here, not in the
// transport: an author may only touch their own and only while it waits, and a
// decision only applies to something still waiting.
package suggestion

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"loudbot/internal/comment"
)

// Status is where a suggestion stands.
type Status string

const (
	StatusPending Status = "pending"
	// StatusApproved is the moment between a moderator's yes and the send.
	StatusApproved Status = "approved"
	// StatusPublished is in the channel.
	StatusPublished Status = "published"
	StatusDeclined  Status = "declined"
	StatusWithdrawn Status = "withdrawn"
	// StatusFailed is Telegram refusing the post after it was approved.
	StatusFailed Status = "failed"
)

var (
	// ErrNotFound is a suggestion the repository does not hold.
	ErrNotFound = errors.New("suggestion not found")
	// ErrNotYours guards one author's suggestion from another's commands, which
	// carry an id back from the client and cannot be trusted on their own.
	ErrNotYours = errors.New("suggestion belongs to someone else")
	// ErrNotPending is an edit, a withdrawal or a decision arriving too late.
	ErrNotPending = errors.New("suggestion is no longer waiting")
	ErrEmptyPost  = errors.New("post has neither text nor media")
	ErrTooLong    = errors.New("post text is too long")
	ErrNoDraft    = errors.New("the bot is not waiting for a post")
	// ErrTooManyPending is one account holding more of the moderators' time than allowed.
	ErrTooManyPending = errors.New("too many posts already waiting for a decision")
)

// Suggestion is one post offered by a reader.
type Suggestion struct {
	ID     int64
	UserID int64
	Text   string
	Media  []comment.Media
	Status Status
	// ModerationMessageID is the card in the moderation chat; an edit rewrites it
	// in place rather than adding a second one.
	ModerationMessageID int
	// ChannelMessageID is the published post, set once it reaches the channel.
	ChannelMessageID int
	CreatedAt        time.Time
	EditedAt         *time.Time
}

// Draft is the bot waiting for the text of a post: a new one, or a rewrite.
type Draft struct {
	UserID    int64
	EditingID int64
	CreatedAt time.Time
}

// Editing reports whether the awaited text replaces an existing suggestion.
func (d Draft) Editing() bool {
	return d.EditingID != 0
}

type Repository interface {
	EnsureUser(ctx context.Context, userID int64) (comment.User, error)

	CreateSuggestion(ctx context.Context, s Suggestion) (int64, error)
	Suggestion(ctx context.Context, id int64) (Suggestion, error)
	SuggestionsByStatus(ctx context.Context, userID int64, statuses ...Status) ([]Suggestion, error)
	CountSuggestions(ctx context.Context, userID int64, statuses ...Status) (int, error)
	UpdateSuggestionText(ctx context.Context, id int64, text string, media []comment.Media, at time.Time) error
	SetSuggestionStatus(ctx context.Context, id int64, status Status, decidedAt time.Time) error
	SetModerationMessage(ctx context.Context, id int64, messageID int) error
	SetChannelMessage(ctx context.Context, id int64, messageID int) error

	SaveSuggestionDraft(ctx context.Context, draft Draft) error
	SuggestionDraft(ctx context.Context, userID int64) (Draft, error)
	DeleteSuggestionDraft(ctx context.Context, userID int64) error
}

// Options are the tunables the service reads from config.
type Options struct {
	MaxTextLen int
	// PendingLimit caps how many posts one account may have awaiting a decision,
	// so a single person cannot bury the moderators. Zero means no limit.
	PendingLimit int
}

const defaultMaxTextLen = 3500

type Service struct {
	repo Repository
	opts Options
	now  func() time.Time
}

func New(repo Repository, opts Options) *Service {
	if opts.MaxTextLen <= 0 {
		opts.MaxTextLen = defaultMaxTextLen
	}

	return &Service{repo: repo, opts: opts, now: time.Now}
}

// SetClock replaces the time source; used by tests.
func (s *Service) SetClock(now func() time.Time) {
	s.now = now
}

// Begin puts the bot into "waiting for a post" for this author. editingID is the
// suggestion being rewritten, or zero for a new one.
func (s *Service) Begin(ctx context.Context, userID, editingID int64) error {
	if _, err := s.repo.EnsureUser(ctx, userID); err != nil {
		return fmt.Errorf("ensure user: %w", err)
	}

	if editingID != 0 {
		if _, err := s.own(ctx, userID, editingID); err != nil {
			return err
		}
	} else if err := s.withinLimit(ctx, userID); err != nil {
		// Said early, so nobody writes a post only to be turned away after.
		return err
	}

	draft := Draft{UserID: userID, EditingID: editingID, CreatedAt: s.now()}
	if err := s.repo.SaveSuggestionDraft(ctx, draft); err != nil {
		return fmt.Errorf("save suggestion draft: %w", err)
	}

	return nil
}

// Waiting reports the draft this author owes text for.
func (s *Service) Waiting(ctx context.Context, userID int64) (Draft, error) {
	draft, err := s.repo.SuggestionDraft(ctx, userID)
	switch {
	case errors.Is(err, ErrNotFound):
		return Draft{}, ErrNoDraft
	case err != nil:
		return Draft{}, fmt.Errorf("suggestion draft: %w", err)
	}

	return draft, nil
}

// Abandon forgets that the bot was waiting, without touching any suggestion.
func (s *Service) Abandon(ctx context.Context, userID int64) error {
	if err := s.repo.DeleteSuggestionDraft(ctx, userID); err != nil {
		return fmt.Errorf("delete suggestion draft: %w", err)
	}

	return nil
}

// Submit turns the awaited message into a suggestion — a new one, or a rewrite of
// the one the draft names. It reports whether this was an edit, because an edit
// updates the card the moderators already have rather than sending another.
func (s *Service) Submit(ctx context.Context, userID int64, text string, media []comment.Media) (Suggestion, bool, error) {
	draft, err := s.Waiting(ctx, userID)
	if err != nil {
		return Suggestion{}, false, err
	}

	text, err = s.validate(text, media)
	if err != nil {
		return Suggestion{}, false, err
	}

	if draft.Editing() {
		sug, err := s.edit(ctx, userID, draft.EditingID, text, media)
		if err != nil {
			return Suggestion{}, false, err
		}

		if err := s.Abandon(ctx, userID); err != nil {
			return Suggestion{}, false, err
		}

		return sug, true, nil
	}

	// Checked again here, not only in Begin: the limit is about what exists at the
	// moment a row is added, and a draft can sit open for a while.
	if err := s.withinLimit(ctx, userID); err != nil {
		return Suggestion{}, false, err
	}

	sug := Suggestion{
		UserID:    userID,
		Text:      text,
		Media:     media,
		Status:    StatusPending,
		CreatedAt: s.now(),
	}

	id, err := s.repo.CreateSuggestion(ctx, sug)
	if err != nil {
		return Suggestion{}, false, fmt.Errorf("create suggestion: %w", err)
	}
	sug.ID = id

	if err := s.Abandon(ctx, userID); err != nil {
		return Suggestion{}, false, err
	}

	return sug, false, nil
}

func (s *Service) edit(ctx context.Context, userID, id int64, text string, media []comment.Media) (Suggestion, error) {
	sug, err := s.own(ctx, userID, id)
	if err != nil {
		return Suggestion{}, err
	}

	at := s.now()
	if err := s.repo.UpdateSuggestionText(ctx, id, text, media, at); err != nil {
		return Suggestion{}, fmt.Errorf("update suggestion text: %w", err)
	}

	sug.Text = text
	sug.Media = media
	sug.EditedAt = &at

	return sug, nil
}

// Withdraw takes an author's own suggestion off the moderators' hands.
func (s *Service) Withdraw(ctx context.Context, userID, id int64) (Suggestion, error) {
	sug, err := s.own(ctx, userID, id)
	if err != nil {
		return Suggestion{}, err
	}

	if err := s.repo.SetSuggestionStatus(ctx, id, StatusWithdrawn, s.now()); err != nil {
		return Suggestion{}, fmt.Errorf("withdraw suggestion: %w", err)
	}

	sug.Status = StatusWithdrawn

	return sug, nil
}

// Pending lists what this author is still waiting on a decision for.
func (s *Service) Pending(ctx context.Context, userID int64) ([]Suggestion, error) {
	return s.list(ctx, userID, StatusPending)
}

// Published lists this author's suggestions that reached the channel.
func (s *Service) Published(ctx context.Context, userID int64) ([]Suggestion, error) {
	return s.list(ctx, userID, StatusPublished)
}

// withinLimit refuses an account that already has too much awaiting a decision.
func (s *Service) withinLimit(ctx context.Context, userID int64) error {
	if s.opts.PendingLimit <= 0 {
		return nil
	}

	waiting, err := s.repo.CountSuggestions(ctx, userID, StatusPending)
	if err != nil {
		return fmt.Errorf("count pending suggestions: %w", err)
	}

	if waiting >= s.opts.PendingLimit {
		return fmt.Errorf("%w: %d of %d", ErrTooManyPending, waiting, s.opts.PendingLimit)
	}

	return nil
}

func (s *Service) list(ctx context.Context, userID int64, statuses ...Status) ([]Suggestion, error) {
	out, err := s.repo.SuggestionsByStatus(ctx, userID, statuses...)
	if err != nil {
		return nil, fmt.Errorf("list suggestions: %w", err)
	}

	return out, nil
}

// Get loads a suggestion for a moderator, who is not its author.
func (s *Service) Get(ctx context.Context, id int64) (Suggestion, error) {
	sug, err := s.repo.Suggestion(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Suggestion{}, ErrNotFound
		}

		return Suggestion{}, fmt.Errorf("suggestion: %w", err)
	}

	return sug, nil
}

// Approve accepts a waiting suggestion. Publishing is the transport's job;
// MarkPublished or MarkFailed records how that went.
func (s *Service) Approve(ctx context.Context, id int64) (Suggestion, error) {
	sug, err := s.pending(ctx, id)
	if err != nil {
		return Suggestion{}, err
	}

	if err := s.repo.SetSuggestionStatus(ctx, id, StatusApproved, s.now()); err != nil {
		return Suggestion{}, fmt.Errorf("approve suggestion: %w", err)
	}

	sug.Status = StatusApproved

	return sug, nil
}

// Decline marks a waiting suggestion refused.
func (s *Service) Decline(ctx context.Context, id int64) (Suggestion, error) {
	sug, err := s.pending(ctx, id)
	if err != nil {
		return Suggestion{}, err
	}

	if err := s.repo.SetSuggestionStatus(ctx, id, StatusDeclined, s.now()); err != nil {
		return Suggestion{}, fmt.Errorf("decline suggestion: %w", err)
	}

	sug.Status = StatusDeclined

	return sug, nil
}

// MarkPublished records the post an approved suggestion became.
func (s *Service) MarkPublished(ctx context.Context, id int64, channelMessageID int) error {
	if err := s.repo.SetChannelMessage(ctx, id, channelMessageID); err != nil {
		return fmt.Errorf("record published post: %w", err)
	}

	if err := s.repo.SetSuggestionStatus(ctx, id, StatusPublished, s.now()); err != nil {
		return fmt.Errorf("mark suggestion published: %w", err)
	}

	return nil
}

// MarkFailed records that Telegram refused an already approved post.
func (s *Service) MarkFailed(ctx context.Context, id int64) error {
	if err := s.repo.SetSuggestionStatus(ctx, id, StatusFailed, s.now()); err != nil {
		return fmt.Errorf("mark suggestion failed: %w", err)
	}

	return nil
}

// AttachModerationCard remembers the card the moderators see.
func (s *Service) AttachModerationCard(ctx context.Context, id int64, messageID int) error {
	if err := s.repo.SetModerationMessage(ctx, id, messageID); err != nil {
		return fmt.Errorf("attach moderation card: %w", err)
	}

	return nil
}

// own loads a suggestion that this author may still change.
func (s *Service) own(ctx context.Context, userID, id int64) (Suggestion, error) {
	sug, err := s.Get(ctx, id)
	if err != nil {
		return Suggestion{}, err
	}

	if sug.UserID != userID {
		return Suggestion{}, fmt.Errorf("%w: %d", ErrNotYours, id)
	}

	if sug.Status != StatusPending {
		return Suggestion{}, fmt.Errorf("%w: %d is %s", ErrNotPending, id, sug.Status)
	}

	return sug, nil
}

// pending loads a suggestion a decision may still be applied to.
func (s *Service) pending(ctx context.Context, id int64) (Suggestion, error) {
	sug, err := s.Get(ctx, id)
	if err != nil {
		return Suggestion{}, err
	}

	if sug.Status != StatusPending {
		return Suggestion{}, fmt.Errorf("%w: %d is %s", ErrNotPending, id, sug.Status)
	}

	return sug, nil
}

func (s *Service) validate(text string, media []comment.Media) (string, error) {
	text = strings.TrimSpace(text)

	if text == "" && len(media) == 0 {
		return "", ErrEmptyPost
	}

	if n := utf8.RuneCountInString(text); n > s.opts.MaxTextLen {
		return "", fmt.Errorf("%w: %d > %d", ErrTooLong, n, s.opts.MaxTextLen)
	}

	return text, nil
}
