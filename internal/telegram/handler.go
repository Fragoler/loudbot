package telegram

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"strings"
	"unicode/utf8"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"loudbot/internal/achievement"
	"loudbot/internal/comment"
	"loudbot/internal/config"
	"loudbot/internal/profile"
	"loudbot/internal/suggestion"
)

const (
	startCommand     = "/start"
	profileCommand   = "/profile"
	suggestCommand   = "/suggest"
	pendingCommand   = "/pending"
	publishedCommand = "/posts"
	// quoteLimit keeps the quoted post short enough to stay a hint rather than a
	// wall of text above the author's own message.
	quoteLimit = 280
)

func (b *Bot) handleUpdate(ctx context.Context, _ *tgbot.Bot, update *models.Update) {
	switch {
	case update.CallbackQuery != nil:
		b.onCallback(ctx, update.CallbackQuery)
	case update.Message != nil && update.Message.IsAutomaticForward:
		b.onDiscussionForward(ctx, update.Message)
	case update.Message != nil && update.Message.Chat.Type == models.ChatTypePrivate:
		b.onPrivateMessage(ctx, update.Message)
	}
}

// onDiscussionForward is where a new post really becomes commentable: the copy
// Telegram auto-forwards here is the anchor every comment replies to, and the
// bot's own first comment under it carries the button readers tap.
func (b *Bot) onDiscussionForward(ctx context.Context, msg *models.Message) {
	if msg.Chat.ID != b.cfg.Telegram.DiscussionChatID {
		return
	}

	origin := msg.ForwardOrigin
	if origin == nil || origin.MessageOriginChannel == nil {
		return
	}

	channel := origin.MessageOriginChannel
	if channel.Chat.ID != b.cfg.Telegram.ChannelID {
		return
	}

	post := comment.Post{
		ChannelMessageID:    channel.MessageID,
		DiscussionChatID:    msg.Chat.ID,
		DiscussionMessageID: msg.ID,
		Body:                messageBody(msg),
		ChannelUsername:     channel.Chat.Username,
	}

	if err := b.comments.OnPostPublished(ctx, post); err != nil {
		b.log.Error("announce post in discussion thread",
			slog.Int("post_id", post.ChannelMessageID),
			slog.Any("error", err),
		)

		return
	}

	b.log.Info("post announced in discussion thread",
		slog.Int("post_id", post.ChannelMessageID),
		slog.Int("thread_message_id", post.DiscussionMessageID),
	)
}

func (b *Bot) onPrivateMessage(ctx context.Context, msg *models.Message) {
	if msg.From == nil {
		return
	}

	// A deep link or a command opens a new flow, so the previous conversation goes
	// first — including the "/start" Telegram made the user send to get here.
	if payload, ok := startPayload(msg.Text); ok {
		b.wipe(ctx, msg.From.ID, msg.ID)
		b.onStart(ctx, msg, payload)

		return
	}

	switch strings.TrimSpace(msg.Text) {
	case profileCommand:
		b.wipe(ctx, msg.From.ID, msg.ID)
		b.onProfile(ctx, msg)

		return
	case suggestCommand:
		b.wipe(ctx, msg.From.ID, msg.ID)
		b.onSuggest(ctx, msg)

		return
	case pendingCommand:
		b.wipe(ctx, msg.From.ID, msg.ID)
		b.onPendingPosts(ctx, msg)

		return
	case publishedCommand:
		b.wipe(ctx, msg.From.ID, msg.ID)
		b.onPublishedPosts(ctx, msg)

		return
	}

	// A post the bot asked for outranks the comment flow: the author was told to
	// write one, and nothing else in this chat is waiting on them.
	if _, err := b.suggestions.Waiting(ctx, msg.From.ID); err == nil {
		b.remember(ctx, msg.From.ID, msg.ID)
		b.onPostText(ctx, msg)

		return
	}

	b.remember(ctx, msg.From.ID, msg.ID)
	b.onComment(ctx, msg)
}

// wipe clears the private chat before a new flow draws its first message.
func (b *Bot) wipe(ctx context.Context, userID int64, extra ...int) {
	if err := b.history.Wipe(ctx, userID, extra...); err != nil {
		b.log.Warn("wipe private chat", slog.Int64("user_id", userID), slog.Any("error", err))
	}
}

func (b *Bot) remember(ctx context.Context, userID int64, messageIDs ...int) {
	if err := b.history.Record(ctx, userID, messageIDs...); err != nil {
		b.log.Warn("record private message", slog.Int64("user_id", userID), slog.Any("error", err))
	}
}

// onStart greets an author arriving from a post: it quotes the post, links back
// to it, and asks for the comment. The mask is chosen later.
func (b *Bot) onStart(ctx context.Context, msg *models.Message, payload string) {
	userID := msg.From.ID

	if payload == "" {
		b.reply(ctx, msg.Chat.ID, b.cfg.Messages.Help)

		return
	}

	started, err := b.comments.Start(ctx, userID, payload)
	if err != nil {
		b.replyError(ctx, msg.Chat.ID, "start comment draft", userID, err)

		return
	}

	params := &tgbot.SendMessageParams{
		ChatID:    msg.Chat.ID,
		Text:      startPrompt(started, b.cfg.Messages),
		ParseMode: models.ParseModeHTML,
	}

	if started.Link != "" {
		params.ReplyMarkup = models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{{
				{Text: b.cfg.Messages.ButtonOpenPost, URL: started.Link},
			}},
		}
	}

	prompt, err := b.api.SendMessage(ctx, params)
	if err != nil {
		b.log.Error("send comment prompt", slog.Int64("user_id", userID), slog.Any("error", err))

		return
	}

	b.remember(ctx, userID, prompt.ID)
}

func (b *Bot) onComment(ctx context.Context, msg *models.Message) {
	userID := msg.From.ID

	media, err := extractMedia(msg)
	if err != nil {
		b.reply(ctx, msg.Chat.ID, b.cfg.Messages.Errors.Unsupported)

		return
	}

	text := msg.Text
	if text == "" {
		text = msg.Caption
	}

	staged, err := b.comments.Stage(ctx, comment.StageRequest{
		UserID:    userID,
		MessageID: msg.ID,
		Text:      text,
		Media:     media,
	})
	if err != nil {
		b.replyError(ctx, msg.Chat.ID, "stage comment", userID, err)

		return
	}

	// Writing again instead of picking a mask replaces the previous draft, so the
	// keyboard that belonged to it goes away with it.
	b.deleteMessage(ctx, msg.Chat.ID, staged.StalePromptID)

	prompt, err := b.api.SendMessage(ctx, &tgbot.SendMessageParams{
		ChatID:      msg.Chat.ID,
		Text:        b.cfg.Messages.ChooseNickname,
		ReplyMarkup: nicknameKeyboard(staged.Nicknames, b.cfg.Messages.ButtonCancel),
	})
	if err != nil {
		b.log.Error("send nickname keyboard", slog.Int64("user_id", userID), slog.Any("error", err))

		return
	}

	b.remember(ctx, userID, prompt.ID)

	if err := b.comments.AttachPrompt(ctx, userID, prompt.ID); err != nil {
		b.log.Error("attach nickname keyboard", slog.Int64("user_id", userID), slog.Any("error", err))
	}
}

// onProfile shows what the author has earned and what it unlocked.
func (b *Bot) onProfile(ctx context.Context, msg *models.Message) {
	userID := msg.From.ID

	got, err := b.profiles.Get(ctx, userID)
	if err != nil {
		b.log.Error("build profile", slog.Int64("user_id", userID), slog.Any("error", err))
		b.reply(ctx, msg.Chat.ID, b.cfg.Messages.Errors.Internal)

		return
	}

	b.replyHTML(ctx, msg.Chat.ID, profileText(got, b.cfg.Messages.Profile))
}

// profileText renders the /profile screen. Every line is escaped — the wording
// comes from config and the titles and masks from the database — and the markup
// is added here, so neither source can break Telegram's HTML.
func profileText(p profile.Profile, m config.Profile) string {
	var b strings.Builder

	b.WriteString(bold(m.Title))
	b.WriteString("\n\n")
	b.WriteString(esc(fmt.Sprintf(m.Comments, p.Activity.Comments)))
	b.WriteString("\n")
	b.WriteString(esc(fmt.Sprintf(m.Replies, p.Activity.Replies)))
	b.WriteString("\n")
	b.WriteString(esc(fmt.Sprintf(m.Posts, p.Activity.Posts)))

	// The in-flight line only matters when something is in flight.
	if p.Activity.PostsPending > 0 {
		b.WriteString("\n")
		b.WriteString(esc(fmt.Sprintf(m.PostsPending, p.Activity.PostsPending)))
	}

	b.WriteString("\n\n")
	b.WriteString(bold(withCount(m.AchievementsHead, len(p.Achievements))))
	b.WriteString("\n")

	if len(p.Achievements) == 0 {
		b.WriteString("<i>" + esc(m.NoAchievements) + "</i>\n")
	}

	for _, a := range p.Achievements {
		b.WriteString("• <b>" + esc(a.Title) + "</b>\n")
		if a.Description != "" {
			b.WriteString("   <i>" + esc(a.Description) + "</i>\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(bold(withCount(m.NicknamesHead, len(p.Nicknames))))
	b.WriteString("\n")

	for _, n := range p.Nicknames {
		b.WriteString("• " + esc(n.Label) + "\n")
	}

	return strings.TrimRight(b.String(), "\n")
}

// achievementText is the notice a freshly earned achievement sends.
func achievementText(head string, granted achievement.Granted) string {
	text := bold(head) + "\n\n" + "<b>" + esc(granted.Title) + "</b>"
	if granted.Description != "" {
		text += "\n<i>" + esc(granted.Description) + "</i>"
	}

	return text
}

// withCount appends "(n)" to a heading, so a list says how long it is. An empty
// list says so in words right below, and "(0)" beside it only repeats that.
func withCount(head string, n int) string {
	if n == 0 {
		return head
	}

	return fmt.Sprintf("%s (%d)", head, n)
}

func bold(text string) string {
	return "<b>" + esc(text) + "</b>"
}

func esc(text string) string {
	return html.EscapeString(text)
}

func (b *Bot) onCallback(ctx context.Context, query *models.CallbackQuery) {
	switch {
	case suggestion.Owns(query.Data):
		b.onSuggestionCallback(ctx, query)
	case query.Data == comment.CancelCallback:
		b.onCancel(ctx, query)
	case strings.HasPrefix(query.Data, "nick:"):
		b.onNicknameChosen(ctx, query)
	default:
		b.answer(ctx, query.ID, "")
	}
}

// onNicknameChosen signs the staged message and sends it into the thread.
func (b *Bot) onNicknameChosen(ctx context.Context, query *models.CallbackQuery) {
	label, err := comment.ParseNicknameCallback(query.Data)
	if err != nil {
		b.answer(ctx, query.ID, b.cfg.Messages.Errors.BadPayload)

		return
	}

	published, err := b.comments.Publish(ctx, query.From.ID, label)
	if err != nil {
		b.logCoreError("publish comment", query.From.ID, err)
		b.answer(ctx, query.ID, userMessage(b.cfg.Messages, err))

		return
	}

	b.log.Info("comment published",
		slog.Int64("comment_id", published.ID),
		slog.Int("post_id", published.PostID),
		slog.Int("message_id", published.MessageID),
	)

	b.answer(ctx, query.ID, b.cfg.Messages.Published)

	// The keyboard has done its job; turning it into the confirmation keeps the
	// private chat from filling up with dead buttons.
	b.replacePrompt(ctx, query, b.cfg.Messages.Published)

	b.notifyReply(ctx, published)

	b.award(ctx, achievement.Event{
		Kind:    achievement.KindComment,
		UserID:  published.UserID,
		Text:    published.Text,
		At:      published.CreatedAt,
		IsReply: published.ReplyToCommentID != 0,
	})
}

// notifyReply tells a comment's author that someone answered it.
func (b *Bot) notifyReply(ctx context.Context, reply comment.Comment) {
	notice, ok, err := b.comments.ReplyNotice(ctx, reply)
	if err != nil {
		b.log.Error("build reply notice", slog.Int64("comment_id", reply.ID), slog.Any("error", err))

		return
	}

	if !ok {
		return
	}

	markup := models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{{
			{Text: b.cfg.Messages.ButtonOpenComment, URL: notice.Link},
		}},
	}

	b.sendWithKeyboard(ctx, notice.RecipientID, replyNoticeText(b.cfg.Messages.ReplyNotice, notice), markup)
}

func replyNoticeText(head string, notice comment.ReplyNotice) string {
	text := bold(head) + "\n\n" + bold(notice.Nickname)
	if body := strings.TrimSpace(notice.Text); body != "" {
		text += "\n" + esc(body)
	}

	return text
}

// award hands one event to the achievements layer and tells the author what it
// earned. Every trigger lives in internal/achievement; none of it here.
func (b *Bot) award(ctx context.Context, event achievement.Event) {
	granted, err := b.awards.Award(ctx, event)
	if err != nil {
		// Whatever earned this is already public; a failure here must not look
		// like that failed.
		b.log.Error("award achievements",
			slog.Int64("user_id", event.UserID),
			slog.String("kind", string(event.Kind)),
			slog.Any("error", err),
		)
	}

	for _, a := range granted {
		b.replyHTML(ctx, event.UserID, achievementText(b.cfg.Messages.Achievement, a))
	}
}

// onCancel drops the staged message, taking both it and the keyboard off-screen.
func (b *Bot) onCancel(ctx context.Context, query *models.CallbackQuery) {
	cancelled, err := b.comments.Cancel(ctx, query.From.ID)
	if err != nil {
		b.logCoreError("cancel comment", query.From.ID, err)
		b.answer(ctx, query.ID, userMessage(b.cfg.Messages, err))

		return
	}

	b.answer(ctx, query.ID, b.cfg.Messages.Cancelled)

	b.deleteMessage(ctx, cancelled.ChatID, cancelled.UserMessageID)
	b.deleteMessage(ctx, cancelled.ChatID, cancelled.PromptMessageID)
}

// replacePrompt rewrites the keyboard message in place, dropping its buttons.
func (b *Bot) replacePrompt(ctx context.Context, query *models.CallbackQuery, text string) {
	if query.Message.Message == nil {
		return
	}

	_, err := b.api.EditMessageText(ctx, &tgbot.EditMessageTextParams{
		ChatID:    query.Message.Message.Chat.ID,
		MessageID: query.Message.Message.ID,
		Text:      text,
	})
	if err != nil {
		b.log.Debug("replace nickname keyboard", slog.Any("error", err))
	}
}

func (b *Bot) deleteMessage(ctx context.Context, chatID int64, messageID int) {
	if messageID == 0 {
		return
	}

	_, err := b.api.DeleteMessage(ctx, &tgbot.DeleteMessageParams{ChatID: chatID, MessageID: messageID})
	if err != nil {
		b.log.Debug("delete message",
			slog.Int64("chat_id", chatID),
			slog.Int("message_id", messageID),
			slog.Any("error", err),
		)

		return
	}

	b.forget(ctx, chatID, messageID)
}

func (b *Bot) forget(ctx context.Context, userID int64, messageIDs ...int) {
	if err := b.history.Forget(ctx, userID, messageIDs...); err != nil {
		b.log.Debug("forget private message", slog.Int64("user_id", userID), slog.Any("error", err))
	}
}

// startPayload splits "/start <payload>"; ok is false for any other text.
func startPayload(text string) (string, bool) {
	text = strings.TrimSpace(text)
	if text != startCommand && !strings.HasPrefix(text, startCommand+" ") {
		return "", false
	}

	return strings.TrimSpace(strings.TrimPrefix(text, startCommand)), true
}

// startPrompt asks for the text, quoting whatever the author is answering — the
// post, or the comment they opened through its "ответить" link.
func startPrompt(started comment.StartResult, m config.Messages) string {
	if started.IsReply() {
		return promptText(started.ReplyTo.Nickname+": "+started.ReplyTo.Text, m.ReplyPrompt)
	}

	return promptText(started.Post.Body, m.Prompt)
}

// promptText puts a short quote of the source above the instruction.
func promptText(source, ask string) string {
	quote := truncate(strings.TrimSpace(source), quoteLimit)
	if quote == "" {
		return ask
	}

	return "<blockquote>" + html.EscapeString(quote) + "</blockquote>\n" + ask
}

func truncate(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}

	return string([]rune(s)[:limit]) + "…"
}

// nicknameKeyboard lays the masks out two per row, with cancel on its own row.
func nicknameKeyboard(nicknames []comment.Nickname, cancelLabel string) models.InlineKeyboardMarkup {
	const perRow = 2

	rows := make([][]models.InlineKeyboardButton, 0, len(nicknames)/perRow+2)
	row := make([]models.InlineKeyboardButton, 0, perRow)

	for _, n := range nicknames {
		row = append(row, models.InlineKeyboardButton{
			Text:         n.Label,
			CallbackData: comment.NicknameCallback(n.Label),
		})

		if len(row) == perRow {
			rows = append(rows, row)
			row = make([]models.InlineKeyboardButton, 0, perRow)
		}
	}

	if len(row) > 0 {
		rows = append(rows, row)
	}

	rows = append(rows, []models.InlineKeyboardButton{
		{Text: cancelLabel, CallbackData: comment.CancelCallback},
	})

	return models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// messageBody is the text of a message, or the caption when it carries media.
func messageBody(msg *models.Message) string {
	if msg.Text != "" {
		return msg.Text
	}

	return msg.Caption
}

// extractMedia pulls the reusable file ids out of a message. Telegram keeps the
// files, so the bot only ever stores and re-sends their ids.
func extractMedia(msg *models.Message) ([]comment.Media, error) {
	switch {
	case len(msg.Photo) > 0:
		// The last size is the largest one Telegram offers.
		largest := msg.Photo[len(msg.Photo)-1]

		return []comment.Media{{
			Type:         comment.MediaPhoto,
			FileID:       largest.FileID,
			FileUniqueID: largest.FileUniqueID,
		}}, nil

	case msg.Video != nil:
		return []comment.Media{{
			Type:         comment.MediaVideo,
			FileID:       msg.Video.FileID,
			FileUniqueID: msg.Video.FileUniqueID,
		}}, nil

	case msg.Document != nil:
		return []comment.Media{{
			Type:         comment.MediaDocument,
			FileID:       msg.Document.FileID,
			FileUniqueID: msg.Document.FileUniqueID,
		}}, nil

	case msg.Text != "":
		return nil, nil

	case msg.Sticker != nil, msg.Voice != nil, msg.Audio != nil, msg.VideoNote != nil, msg.Animation != nil:
		return nil, fmt.Errorf("unsupported attachment")

	default:
		return nil, nil
	}
}

// reply answers in the private chat, where chatID is also the user id.
func (b *Bot) reply(ctx context.Context, chatID int64, text string) {
	b.send(ctx, chatID, text, "")
}

// replyHTML answers with markup; the caller must have escaped everything it did
// not write itself.
func (b *Bot) replyHTML(ctx context.Context, chatID int64, text string) {
	b.send(ctx, chatID, text, models.ParseModeHTML)
}

func (b *Bot) send(ctx context.Context, chatID int64, text string, mode models.ParseMode) {
	sent, err := b.api.SendMessage(ctx, &tgbot.SendMessageParams{
		ChatID:    chatID,
		Text:      text,
		ParseMode: mode,
	})
	if err != nil {
		b.log.Error("send reply", slog.Int64("chat_id", chatID), slog.Any("error", err))

		return
	}

	b.remember(ctx, chatID, sent.ID)
}

func (b *Bot) replyError(ctx context.Context, chatID int64, op string, userID int64, err error) {
	b.logCoreError(op, userID, err)
	b.reply(ctx, chatID, userMessage(b.cfg.Messages, err))
}

func (b *Bot) logCoreError(op string, userID int64, err error) {
	if expected(err) {
		b.log.Debug(op+" refused", slog.Int64("user_id", userID), slog.Any("reason", err))

		return
	}

	b.log.Error(op+" failed", slog.Int64("user_id", userID), slog.Any("error", err))
}

func (b *Bot) answer(ctx context.Context, queryID, text string) {
	_, err := b.api.AnswerCallbackQuery(ctx, &tgbot.AnswerCallbackQueryParams{
		CallbackQueryID: queryID,
		Text:            text,
	})
	if err != nil {
		b.log.Debug("answer callback", slog.Any("error", err))
	}
}
