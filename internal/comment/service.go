package comment

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultMaxTextLen = 3500
	defaultDraftTTL   = time.Hour
)

// Options are the tunables the service reads from config.
type Options struct {
	BotUsername string
	// ChannelID is needed to build a /c/ link back to a post in a private channel.
	ChannelID int64
	// ReplyLinkText is the wording of the link under every published comment.
	ReplyLinkText string
	MaxTextLen    int
	// DraftTTL bounds how long a deep-link tap stays valid, so that an old draft
	// cannot silently attach a new message to a stale post.
	DraftTTL time.Duration
}

func (o Options) withDefaults() Options {
	if o.MaxTextLen <= 0 {
		o.MaxTextLen = defaultMaxTextLen
	}

	if o.DraftTTL <= 0 {
		o.DraftTTL = defaultDraftTTL
	}

	return o
}

// Service is the core of the comment flow. An author taps the button under a post,
// writes a message, and only then picks the mask it goes out under.
type Service struct {
	repo  Repository
	pub   Publisher
	guard Guard
	opts  Options
	now   func() time.Time
}

// New builds the service; guard may be nil, which means "allow everything".
func New(repo Repository, pub Publisher, guard Guard, opts Options) *Service {
	if guard == nil {
		guard = AllowAll{}
	}

	return &Service{
		repo:  repo,
		pub:   pub,
		guard: guard,
		opts:  opts.withDefaults(),
		now:   time.Now,
	}
}

// SetClock replaces the time source; used by tests around draft expiry.
func (s *Service) SetClock(now func() time.Time) {
	s.now = now
}

// DeepLink is the URL for the button under a channel post.
func (s *Service) DeepLink(channelMessageID int) string {
	return DeepLink(s.opts.BotUsername, channelMessageID)
}

// StartResult is what the bot shows the moment an author opens it from a post or
// from the "ответить" link under a comment.
type StartResult struct {
	Post Post
	// Link points back at the post itself.
	Link string
	// ReplyTo is the comment being answered; its ID is zero for a plain comment.
	ReplyTo Comment
}

// IsReply reports whether the author arrived to answer a comment.
func (r StartResult) IsReply() bool {
	return r.ReplyTo.ID != 0
}

// Start binds the author to what they tapped — a post, or a comment to answer —
// and invites them to write. It does not ask for a mask yet: that happens once
// there is something to sign.
func (s *Service) Start(ctx context.Context, userID int64, payload string) (StartResult, error) {
	target, err := ParseStartPayload(payload)
	if err != nil {
		return StartResult{}, err
	}

	if _, err := s.activeUser(ctx, userID); err != nil {
		return StartResult{}, err
	}

	if _, err := s.Nicknames(ctx, userID); err != nil {
		return StartResult{}, err
	}

	var result StartResult

	postID := target.PostID
	if target.IsReply() {
		parent, err := s.comment(ctx, target.CommentID)
		if err != nil {
			return StartResult{}, err
		}

		postID = parent.PostID
		result.ReplyTo = parent
	}

	post, err := s.post(ctx, postID)
	if err != nil {
		return StartResult{}, err
	}

	draft := Draft{
		UserID:           userID,
		PostID:           postID,
		ReplyToCommentID: target.CommentID,
		CreatedAt:        s.now(),
	}
	if err := s.repo.SaveDraft(ctx, draft); err != nil {
		return StartResult{}, fmt.Errorf("save draft: %w", err)
	}

	result.Post = post
	result.Link = PostLink(post, s.opts.ChannelID)

	return result, nil
}

// comment loads a comment that is still available to answer.
func (s *Service) comment(ctx context.Context, id int64) (Comment, error) {
	c, err := s.repo.Comment(ctx, id)
	switch {
	case errors.Is(err, ErrNotFound):
		return Comment{}, fmt.Errorf("%w: %d", ErrUnknownComment, id)
	case err != nil:
		return Comment{}, fmt.Errorf("comment: %w", err)
	}

	if c.Status != StatusPublished || c.MessageID == 0 {
		return Comment{}, fmt.Errorf("%w: %d is not published", ErrUnknownComment, id)
	}

	return c, nil
}

// StageRequest is a message the author sent to the bot in private.
type StageRequest struct {
	UserID int64
	// MessageID is the author's message, remembered so that Cancel can remove it.
	MessageID int
	Text      string
	Media     []Media
}

// StageResult tells the transport what to draw, and what to clean up first.
type StageResult struct {
	Nicknames []Nickname
	// StaleMessageID and StalePromptID belong to a message the author replaced by
	// writing again instead of picking a mask. Zero when there was nothing staged.
	StaleMessageID int
	StalePromptID  int
}

// Stage records what the author wrote and asks for a mask. Writing again before
// picking one replaces the staged message rather than queueing a second comment.
func (s *Service) Stage(ctx context.Context, req StageRequest) (StageResult, error) {
	user, err := s.activeUser(ctx, req.UserID)
	if err != nil {
		return StageResult{}, err
	}

	draft, err := s.draft(ctx, req.UserID)
	if err != nil {
		return StageResult{}, err
	}

	text, err := s.validateContent(req.Text, req.Media)
	if err != nil {
		return StageResult{}, err
	}

	if _, err := s.post(ctx, draft.PostID); err != nil {
		return StageResult{}, err
	}

	nicknames, err := s.Nicknames(ctx, req.UserID)
	if err != nil {
		return StageResult{}, err
	}

	guardReq := GuardRequest{
		UserID:   req.UserID,
		PostID:   draft.PostID,
		Text:     text,
		HasMedia: len(req.Media) > 0,
	}
	if err := s.guard.Check(ctx, guardReq); err != nil {
		return StageResult{}, err
	}

	stale := StageResult{StaleMessageID: draft.UserMessageID, StalePromptID: draft.PromptMessageID}

	draft.Body = text
	draft.Media = req.Media
	draft.UserMessageID = req.MessageID
	draft.PromptMessageID = 0

	if err := s.repo.SaveDraft(ctx, draft); err != nil {
		return StageResult{}, fmt.Errorf("save draft: %w", err)
	}

	stale.Nicknames = orderNicknames(nicknames, user.LastNickname)

	return stale, nil
}

// AttachPrompt remembers the mask keyboard the transport has just sent, so that
// Cancel — or a replacement message — can take it down again.
func (s *Service) AttachPrompt(ctx context.Context, userID int64, promptMessageID int) error {
	draft, err := s.draft(ctx, userID)
	if err != nil {
		return err
	}

	draft.PromptMessageID = promptMessageID
	if err := s.repo.SaveDraft(ctx, draft); err != nil {
		return fmt.Errorf("save draft: %w", err)
	}

	return nil
}

// Publish signs the staged message with the chosen mask and sends it into the
// post's thread. The post binding survives, so the author can write again.
func (s *Service) Publish(ctx context.Context, userID int64, label string) (Comment, error) {
	if _, err := s.activeUser(ctx, userID); err != nil {
		return Comment{}, err
	}

	draft, err := s.draft(ctx, userID)
	if err != nil {
		return Comment{}, err
	}

	if !draft.Staged() {
		return Comment{}, ErrNothingStaged
	}

	post, err := s.post(ctx, draft.PostID)
	if err != nil {
		return Comment{}, err
	}

	nickname, err := s.Nickname(ctx, userID, label)
	if err != nil {
		return Comment{}, err
	}

	// A reply hangs off the comment it answers; a plain comment off the post's
	// forwarded copy, which is what puts it in the thread at all.
	replyTo := post.DiscussionMessageID
	if draft.ReplyToCommentID != 0 {
		parent, err := s.comment(ctx, draft.ReplyToCommentID)
		if err != nil {
			return Comment{}, err
		}

		replyTo = parent.MessageID
	}

	c := Comment{
		UserID:           userID,
		PostID:           draft.PostID,
		Nickname:         nickname.Label,
		ReplyToCommentID: draft.ReplyToCommentID,
		Text:             draft.Body,
		Media:            draft.Media,
		Status:           StatusPending,
		CreatedAt:        s.now(),
	}

	// The row is created before the message is sent because its own id goes into
	// the "ответить" link the message carries.
	id, err := s.repo.CreateComment(ctx, c)
	if err != nil {
		return Comment{}, fmt.Errorf("create comment: %w", err)
	}
	c.ID = id

	published, err := s.pub.PublishComment(ctx, PublishRequest{
		ChatID:           post.DiscussionChatID,
		ReplyToMessageID: replyTo,
		Text: FormatBody(nickname, draft.Body, ReplyLink{
			URL:  ReplyDeepLink(s.opts.BotUsername, id),
			Text: s.opts.ReplyLinkText,
		}),
		Media: draft.Media,
	})
	if err != nil {
		if markErr := s.repo.MarkCommentFailed(ctx, id); markErr != nil {
			err = errors.Join(err, markErr)
		}

		return Comment{}, fmt.Errorf("publish comment: %w", err)
	}

	if err := s.repo.MarkCommentPublished(ctx, id, published.MessageID); err != nil {
		return Comment{}, fmt.Errorf("mark comment published: %w", err)
	}

	if err := s.repo.SetLastNickname(ctx, userID, nickname.Label); err != nil {
		return Comment{}, fmt.Errorf("set last nickname: %w", err)
	}

	if err := s.clearStaged(ctx, draft); err != nil {
		return Comment{}, err
	}

	c.MessageID = published.MessageID
	c.Status = StatusPublished

	return c, nil
}

// CancelResult names the two messages the transport removes from the private chat.
type CancelResult struct {
	ChatID          int64
	UserMessageID   int
	PromptMessageID int
}

// Cancel drops the staged message and returns the author to writing. The draft
// keeps its post, so the invitation above is still the one they are answering.
func (s *Service) Cancel(ctx context.Context, userID int64) (CancelResult, error) {
	draft, err := s.draft(ctx, userID)
	if err != nil {
		return CancelResult{}, err
	}

	if !draft.Staged() {
		return CancelResult{}, ErrNothingStaged
	}

	result := CancelResult{
		ChatID:          userID,
		UserMessageID:   draft.UserMessageID,
		PromptMessageID: draft.PromptMessageID,
	}

	if err := s.clearStaged(ctx, draft); err != nil {
		return CancelResult{}, err
	}

	return result, nil
}

func (s *Service) clearStaged(ctx context.Context, draft Draft) error {
	draft.Body = ""
	draft.Media = nil
	draft.UserMessageID = 0
	draft.PromptMessageID = 0

	if err := s.repo.SaveDraft(ctx, draft); err != nil {
		return fmt.Errorf("clear staged draft: %w", err)
	}

	return nil
}

func (s *Service) validateContent(text string, media []Media) (string, error) {
	text = strings.TrimSpace(text)

	if text == "" && len(media) == 0 {
		return "", ErrEmptyComment
	}

	if n := utf8.RuneCountInString(text); n > s.opts.MaxTextLen {
		return "", fmt.Errorf("%w: %d > %d", ErrTextTooLong, n, s.opts.MaxTextLen)
	}

	return text, nil
}

func (s *Service) activeUser(ctx context.Context, userID int64) (User, error) {
	user, err := s.repo.EnsureUser(ctx, userID)
	if err != nil {
		return User{}, fmt.Errorf("ensure user: %w", err)
	}

	if user.Banned {
		return User{}, ErrBanned
	}

	return user, nil
}

// draft returns the open draft, dropping and reporting it once it is older than the TTL.
func (s *Service) draft(ctx context.Context, userID int64) (Draft, error) {
	draft, err := s.repo.Draft(ctx, userID)
	switch {
	case errors.Is(err, ErrNotFound):
		return Draft{}, ErrNoDraft
	case err != nil:
		return Draft{}, fmt.Errorf("draft: %w", err)
	}

	if s.now().Sub(draft.CreatedAt) > s.opts.DraftTTL {
		if err := s.repo.DeleteDraft(ctx, userID); err != nil {
			return Draft{}, fmt.Errorf("delete expired draft: %w", err)
		}

		return Draft{}, ErrDraftExpired
	}

	return draft, nil
}

func (s *Service) post(ctx context.Context, channelMessageID int) (Post, error) {
	post, err := s.repo.Post(ctx, channelMessageID)
	switch {
	case errors.Is(err, ErrNotFound):
		return Post{}, fmt.Errorf("%w: %d", ErrUnknownPost, channelMessageID)
	case err != nil:
		return Post{}, fmt.Errorf("post: %w", err)
	}

	if post.DiscussionMessageID == 0 {
		return Post{}, fmt.Errorf("%w: %d has no discussion thread", ErrUnknownPost, channelMessageID)
	}

	return post, nil
}

// Nickname resolves a label against what this user may actually wear. The check
// is a gate, not a lookup: a callback carries the label back from the client, so
// a mask the user has not unlocked must be refused here.
func (s *Service) Nickname(ctx context.Context, userID int64, label string) (Nickname, error) {
	nicknames, err := s.Nicknames(ctx, userID)
	if err != nil {
		return Nickname{}, err
	}

	for _, n := range nicknames {
		if n.Label == label {
			return n, nil
		}
	}

	return Nickname{}, fmt.Errorf("%w: %q", ErrNicknameUnavailable, label)
}

// Nicknames returns the masks offered to one author: the public ones, plus any
// unlocked by an achievement they hold.
func (s *Service) Nicknames(ctx context.Context, userID int64) ([]Nickname, error) {
	nicknames, err := s.repo.Nicknames(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("nicknames: %w", err)
	}

	if len(nicknames) == 0 {
		return nil, ErrNoNicknames
	}

	return nicknames, nil
}

// orderNicknames moves the author's last mask to the front, leaving the config
// order otherwise intact. It never mutates the configured slice.
func orderNicknames(nicknames []Nickname, last string) []Nickname {
	if last == "" {
		return nicknames
	}

	out := make([]Nickname, 0, len(nicknames))
	for _, n := range nicknames {
		if n.Label == last {
			out = append(out, n)
		}
	}

	if len(out) == 0 {
		return nicknames
	}

	for _, n := range nicknames {
		if n.Label != last {
			out = append(out, n)
		}
	}

	return out
}

// OnPostPublished records the discussion-group anchor of a new channel post and
// leaves the invitation comment under it. Telegram redelivers auto-forwards, so
// the invitation is posted at most once per post.
func (s *Service) OnPostPublished(ctx context.Context, post Post) error {
	existing, err := s.repo.Post(ctx, post.ChannelMessageID)
	switch {
	case err == nil && existing.InviteMessageID != 0:
		return nil
	case err != nil && !errors.Is(err, ErrNotFound):
		return fmt.Errorf("post: %w", err)
	}

	if err := s.repo.LinkPost(ctx, post); err != nil {
		return fmt.Errorf("link post: %w", err)
	}

	invite, err := s.pub.PublishInvite(ctx, InviteRequest{
		ChatID:           post.DiscussionChatID,
		ReplyToMessageID: post.DiscussionMessageID,
		DeepLink:         s.DeepLink(post.ChannelMessageID),
	})
	if err != nil {
		// The post is linked, so comments still work through the button under the
		// post itself; only the invitation is missing.
		return fmt.Errorf("publish invite: %w", err)
	}

	if err := s.repo.MarkInvitePosted(ctx, post.ChannelMessageID, invite.MessageID); err != nil {
		return fmt.Errorf("mark invite posted: %w", err)
	}

	return nil
}

// ReplyNotice is what the author of a comment is told when someone answers it.
type ReplyNotice struct {
	RecipientID int64
	Nickname    string
	Text        string
	// Link opens the thread at the reply itself.
	Link string
}

// ReplyNotice builds the notice for the author of the comment a reply answers.
// ok is false when there is nobody to tell: a top-level comment, or someone
// answering themselves. The recipient learns the mask and the words, never who
// is behind them.
func (s *Service) ReplyNotice(ctx context.Context, reply Comment) (ReplyNotice, bool, error) {
	if reply.ReplyToCommentID == 0 || reply.MessageID == 0 {
		return ReplyNotice{}, false, nil
	}

	parent, err := s.repo.Comment(ctx, reply.ReplyToCommentID)
	if err != nil {
		return ReplyNotice{}, false, fmt.Errorf("parent comment: %w", err)
	}

	if parent.UserID == reply.UserID {
		return ReplyNotice{}, false, nil
	}

	post, err := s.repo.Post(ctx, reply.PostID)
	if err != nil {
		return ReplyNotice{}, false, fmt.Errorf("post: %w", err)
	}

	return ReplyNotice{
		RecipientID: parent.UserID,
		Nickname:    reply.Nickname,
		Text:        reply.Text,
		Link:        ThreadLink(post, s.opts.ChannelID, reply.MessageID),
	}, true, nil
}
