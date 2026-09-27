package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"loudbot/internal/achievement"
	"loudbot/internal/comment"
	"loudbot/internal/config"
	"loudbot/internal/suggestion"
)

// onSuggest asks the author for the text of a new post.
func (b *Bot) onSuggest(ctx context.Context, msg *models.Message) {
	userID := msg.From.ID

	if err := b.suggestions.Begin(ctx, userID, 0); err != nil {
		b.replyError(ctx, msg.Chat.ID, "begin suggestion", userID, err)

		return
	}

	b.reply(ctx, msg.Chat.ID, b.cfg.Messages.Posts.Prompt)
}

// onPostText turns the message the bot was waiting for into a suggestion, or into
// a rewrite of one already on the moderators' hands.
func (b *Bot) onPostText(ctx context.Context, msg *models.Message) {
	userID := msg.From.ID

	media, err := extractMedia(msg)
	if err != nil {
		b.reply(ctx, msg.Chat.ID, b.cfg.Messages.Errors.Unsupported)

		return
	}

	sug, edited, err := b.suggestions.Submit(ctx, userID, messageBody(msg), media)
	if err != nil {
		b.replyError(ctx, msg.Chat.ID, "submit suggestion", userID, err)

		return
	}

	if edited {
		b.updateModerationCard(ctx, sug)
		b.reply(ctx, msg.Chat.ID, b.cfg.Messages.Posts.Edited)

		return
	}

	b.sendModerationCard(ctx, sug)
	b.reply(ctx, msg.Chat.ID, b.cfg.Messages.Posts.Sent)
}

// onPendingPosts lists what the author is waiting on, each with its own buttons.
func (b *Bot) onPendingPosts(ctx context.Context, msg *models.Message) {
	userID := msg.From.ID

	pending, err := b.suggestions.Pending(ctx, userID)
	if err != nil {
		b.replyError(ctx, msg.Chat.ID, "list pending suggestions", userID, err)

		return
	}

	posts := b.cfg.Messages.Posts

	if len(pending) == 0 {
		b.replyHTML(ctx, msg.Chat.ID, bold(posts.PendingHead)+"\n\n<i>"+esc(posts.PendingEmpty)+"</i>")

		return
	}

	b.replyHTML(ctx, msg.Chat.ID, bold(posts.PendingHead))

	for _, sug := range pending {
		b.sendWithKeyboard(ctx, msg.Chat.ID, postCard(sug), authorKeyboard(sug, posts))
	}
}

// onPublishedPosts lists the author's own posts that reached the channel.
func (b *Bot) onPublishedPosts(ctx context.Context, msg *models.Message) {
	userID := msg.From.ID

	published, err := b.suggestions.Published(ctx, userID)
	if err != nil {
		b.replyError(ctx, msg.Chat.ID, "list published suggestions", userID, err)

		return
	}

	posts := b.cfg.Messages.Posts

	if len(published) == 0 {
		b.replyHTML(ctx, msg.Chat.ID, bold(posts.PublishedHead)+"\n\n<i>"+esc(posts.PublishedEmpty)+"</i>")

		return
	}

	b.replyHTML(ctx, msg.Chat.ID, bold(posts.PublishedHead))

	for _, sug := range published {
		markup := models.InlineKeyboardMarkup{}
		if sug.ChannelMessageID != 0 {
			markup.InlineKeyboard = [][]models.InlineKeyboardButton{{
				{Text: posts.OpenPost, URL: b.channelPostLink(sug.ChannelMessageID)},
			}}
		}

		b.sendWithKeyboard(ctx, msg.Chat.ID, postCard(sug), markup)
	}
}

// onSuggestionCallback routes every button that acts on a suggestion.
func (b *Bot) onSuggestionCallback(ctx context.Context, query *models.CallbackQuery) {
	action, id, ok := suggestion.ParseCallback(query.Data)
	if !ok {
		b.answer(ctx, query.ID, b.cfg.Messages.Errors.BadPayload)

		return
	}

	switch action {
	case suggestion.ActionEdit:
		b.onEditPost(ctx, query, id)
	case suggestion.ActionWithdraw:
		b.onWithdrawPost(ctx, query, id)
	case suggestion.ActionApprove:
		b.onAskApprove(ctx, query, id)
	case suggestion.ActionAbort:
		b.onAbortApprove(ctx, query, id)
	case suggestion.ActionConfirm:
		b.onApprovePost(ctx, query, id)
	case suggestion.ActionDecline:
		b.onDeclinePost(ctx, query, id)
	default:
		b.answer(ctx, query.ID, "")
	}
}

func (b *Bot) onEditPost(ctx context.Context, query *models.CallbackQuery, id int64) {
	userID := query.From.ID

	if err := b.suggestions.Begin(ctx, userID, id); err != nil {
		b.logCoreError("begin suggestion edit", userID, err)
		b.answer(ctx, query.ID, userMessage(b.cfg.Messages, err))

		return
	}

	b.answer(ctx, query.ID, "")
	b.reply(ctx, userID, b.cfg.Messages.Posts.EditPrompt)
}

func (b *Bot) onWithdrawPost(ctx context.Context, query *models.CallbackQuery, id int64) {
	userID := query.From.ID

	sug, err := b.suggestions.Withdraw(ctx, userID, id)
	if err != nil {
		b.logCoreError("withdraw suggestion", userID, err)
		b.answer(ctx, query.ID, userMessage(b.cfg.Messages, err))

		return
	}

	b.answer(ctx, query.ID, b.cfg.Messages.Posts.Withdrawn)
	b.retireCard(ctx, query, b.cfg.Messages.Posts.Withdrawn)
	b.closeModerationCard(ctx, sug, b.cfg.Messages.ModerationCard.Withdrawn)
}

// onAskApprove swaps the card's buttons for a confirmation: approving publishes
// to the channel, which cannot be taken back.
func (b *Bot) onAskApprove(ctx context.Context, query *models.CallbackQuery, id int64) {
	card := b.cfg.Messages.ModerationCard

	b.answer(ctx, query.ID, card.Confirm)
	b.swapKeyboard(ctx, query, confirmKeyboard(id, card))
}

// onAbortApprove puts the original buttons back.
func (b *Bot) onAbortApprove(ctx context.Context, query *models.CallbackQuery, id int64) {
	b.answer(ctx, query.ID, "")
	b.swapKeyboard(ctx, query, moderationKeyboard(id, b.cfg.Messages.ModerationCard))
}

// onApprovePost publishes an approved post straight to the channel.
func (b *Bot) onApprovePost(ctx context.Context, query *models.CallbackQuery, id int64) {
	card := b.cfg.Messages.ModerationCard

	sug, err := b.suggestions.Approve(ctx, id)
	if err != nil {
		b.logCoreError("approve suggestion", query.From.ID, err)
		b.answer(ctx, query.ID, userMessage(b.cfg.Messages, err))

		return
	}

	if err := b.releasePost(ctx, sug); err != nil {
		b.answer(ctx, query.ID, card.Failed)
		b.retireCard(ctx, query, card.Failed)

		return
	}

	b.answer(ctx, query.ID, card.Published)
	b.retireCard(ctx, query, card.Published)
}

// releasePost sends an approved post to the channel, records it, tells its author
// and checks what it earned.
func (b *Bot) releasePost(ctx context.Context, sug suggestion.Suggestion) error {
	published, err := b.publishToChannel(ctx, sug)
	if err != nil {
		b.log.Error("publish approved post", slog.Int64("suggestion_id", sug.ID), slog.Any("error", err))

		if markErr := b.suggestions.MarkFailed(ctx, sug.ID); markErr != nil {
			b.log.Error("mark suggestion failed",
				slog.Int64("suggestion_id", sug.ID), slog.Any("error", markErr))
		}

		return err
	}

	if err := b.suggestions.MarkPublished(ctx, sug.ID, published); err != nil {
		b.log.Error("record published post", slog.Int64("suggestion_id", sug.ID), slog.Any("error", err))
	}

	b.log.Info("suggested post published",
		slog.Int64("suggestion_id", sug.ID),
		slog.Int64("user_id", sug.UserID),
		slog.Int("channel_message_id", published),
	)

	b.replyHTML(ctx, sug.UserID, publishedNotice(b.cfg.Messages.Posts, b.channelPostLink(published)))

	b.award(ctx, achievement.Event{
		Kind:   achievement.KindPost,
		UserID: sug.UserID,
		Text:   sug.Text,
		At:     time.Now(),
	})

	return nil
}

// publishedNotice tells an author their post is live, with a link to it.
func publishedNotice(m config.Posts, link string) string {
	text := esc(m.Approved)
	if link != "" {
		text += "\n\n" + `<a href="` + esc(link) + `">` + esc(m.OpenPost) + `</a>`
	}

	return text
}

func (b *Bot) onDeclinePost(ctx context.Context, query *models.CallbackQuery, id int64) {
	card := b.cfg.Messages.ModerationCard

	sug, err := b.suggestions.Decline(ctx, id)
	if err != nil {
		b.logCoreError("decline suggestion", query.From.ID, err)
		b.answer(ctx, query.ID, userMessage(b.cfg.Messages, err))

		return
	}

	b.log.Info("suggested post declined", slog.Int64("suggestion_id", id))

	b.answer(ctx, query.ID, card.Declined)
	b.retireCard(ctx, query, card.Declined)

	b.reply(ctx, sug.UserID, b.cfg.Messages.Posts.Declined)
}

// publishToChannel sends the post as the channel and returns its message id. The
// "comment anonymously" button is attached by the channel-post handler, the same
// way it is for a post written by hand.
func (b *Bot) publishToChannel(ctx context.Context, sug suggestion.Suggestion) (int, error) {
	chatID := b.cfg.Telegram.ChannelID

	if len(sug.Media) == 0 {
		sent, err := b.api.SendMessage(ctx, &tgbot.SendMessageParams{ChatID: chatID, Text: sug.Text})
		if err != nil {
			return 0, err
		}

		return sent.ID, nil
	}

	media := sug.Media[0]
	file := &models.InputFileString{Data: media.FileID}

	switch media.Type {
	case comment.MediaPhoto:
		sent, err := b.api.SendPhoto(ctx, &tgbot.SendPhotoParams{ChatID: chatID, Photo: file, Caption: sug.Text})
		if err != nil {
			return 0, err
		}

		return sent.ID, nil

	case comment.MediaVideo:
		sent, err := b.api.SendVideo(ctx, &tgbot.SendVideoParams{ChatID: chatID, Video: file, Caption: sug.Text})
		if err != nil {
			return 0, err
		}

		return sent.ID, nil

	case comment.MediaDocument:
		sent, err := b.api.SendDocument(ctx, &tgbot.SendDocumentParams{
			ChatID: chatID, Document: file, Caption: sug.Text,
		})
		if err != nil {
			return 0, err
		}

		return sent.ID, nil

	default:
		return 0, fmt.Errorf("unsupported media type %q", media.Type)
	}
}

// sendModerationCard puts a new suggestion in front of the moderators.
func (b *Bot) sendModerationCard(ctx context.Context, sug suggestion.Suggestion) {
	sent, err := b.api.SendMessage(ctx, &tgbot.SendMessageParams{
		ChatID:      b.cfg.Moderation.ChatID,
		Text:        moderationCard(sug, b.cfg.Messages.ModerationCard),
		ParseMode:   models.ParseModeHTML,
		ReplyMarkup: moderationKeyboard(sug.ID, b.cfg.Messages.ModerationCard),
	})
	if err != nil {
		b.log.Error("send moderation card", slog.Int64("suggestion_id", sug.ID), slog.Any("error", err))

		return
	}

	if err := b.suggestions.AttachModerationCard(ctx, sug.ID, sent.ID); err != nil {
		b.log.Error("attach moderation card", slog.Int64("suggestion_id", sug.ID), slog.Any("error", err))
	}
}

// updateModerationCard rewrites the card in place after the author edits, so the
// queue does not grow a second entry for the same suggestion.
func (b *Bot) updateModerationCard(ctx context.Context, sug suggestion.Suggestion) {
	if sug.ModerationMessageID == 0 {
		b.sendModerationCard(ctx, sug)

		return
	}

	_, err := b.api.EditMessageText(ctx, &tgbot.EditMessageTextParams{
		ChatID:      b.cfg.Moderation.ChatID,
		MessageID:   sug.ModerationMessageID,
		Text:        moderationCard(sug, b.cfg.Messages.ModerationCard),
		ParseMode:   models.ParseModeHTML,
		ReplyMarkup: moderationKeyboard(sug.ID, b.cfg.Messages.ModerationCard),
	})
	if err != nil {
		b.log.Error("update moderation card", slog.Int64("suggestion_id", sug.ID), slog.Any("error", err))
	}
}

// closeModerationCard marks a card settled when the decision came from elsewhere,
// such as the author withdrawing the post.
func (b *Bot) closeModerationCard(ctx context.Context, sug suggestion.Suggestion, verdict string) {
	if sug.ModerationMessageID == 0 {
		return
	}

	_, err := b.api.EditMessageText(ctx, &tgbot.EditMessageTextParams{
		ChatID:    b.cfg.Moderation.ChatID,
		MessageID: sug.ModerationMessageID,
		Text:      moderationCard(sug, b.cfg.Messages.ModerationCard) + "\n\n" + bold(verdict),
		ParseMode: models.ParseModeHTML,
	})
	if err != nil {
		b.log.Debug("close moderation card", slog.Int64("suggestion_id", sug.ID), slog.Any("error", err))
	}
}

// retireCard stamps the verdict onto the card the moderator just acted on and
// takes its buttons away, so the decision cannot be tapped twice.
func (b *Bot) retireCard(ctx context.Context, query *models.CallbackQuery, verdict string) {
	if query.Message.Message == nil {
		return
	}

	original := query.Message.Message.Text
	if query.Message.Message.Caption != "" {
		original = query.Message.Message.Caption
	}

	_, err := b.api.EditMessageText(ctx, &tgbot.EditMessageTextParams{
		ChatID:    query.Message.Message.Chat.ID,
		MessageID: query.Message.Message.ID,
		Text:      esc(original) + "\n\n" + bold(verdict),
		ParseMode: models.ParseModeHTML,
	})
	if err != nil {
		b.log.Debug("retire card", slog.Any("error", err))
	}
}

func (b *Bot) swapKeyboard(ctx context.Context, query *models.CallbackQuery, markup models.InlineKeyboardMarkup) {
	if query.Message.Message == nil {
		return
	}

	_, err := b.api.EditMessageReplyMarkup(ctx, &tgbot.EditMessageReplyMarkupParams{
		ChatID:      query.Message.Message.Chat.ID,
		MessageID:   query.Message.Message.ID,
		ReplyMarkup: markup,
	})
	if err != nil {
		b.log.Debug("swap keyboard", slog.Any("error", err))
	}
}

func (b *Bot) sendWithKeyboard(
	ctx context.Context, chatID int64, text string, markup models.InlineKeyboardMarkup,
) {
	params := &tgbot.SendMessageParams{ChatID: chatID, Text: text, ParseMode: models.ParseModeHTML}
	if len(markup.InlineKeyboard) > 0 {
		params.ReplyMarkup = markup
	}

	sent, err := b.api.SendMessage(ctx, params)
	if err != nil {
		b.log.Error("send post card", slog.Int64("chat_id", chatID), slog.Any("error", err))

		return
	}

	b.remember(ctx, chatID, sent.ID)
}

// channelPostLink addresses a post the same way a comment's link does.
func (b *Bot) channelPostLink(messageID int) string {
	return comment.PostLink(
		comment.Post{ChannelMessageID: messageID, ChannelUsername: b.cfg.Telegram.ChannelUsername},
		b.cfg.Telegram.ChannelID,
	)
}

// postCard is one suggestion as its author sees it in a list.
func postCard(sug suggestion.Suggestion) string {
	text := strings.TrimSpace(sug.Text)
	if text == "" {
		text = "—"
	}

	return esc(text)
}

// moderationCard is one suggestion as the moderators see it.
func moderationCard(sug suggestion.Suggestion, m config.ModerationCard) string {
	head := fmt.Sprintf("%s #%d", m.Head, sug.ID)
	if sug.EditedAt != nil {
		head += " · " + m.Edited
	}

	return bold(head) + "\n\n" + postCard(sug)
}

func authorKeyboard(sug suggestion.Suggestion, m config.Posts) models.InlineKeyboardMarkup {
	return models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{{
			{Text: m.ButtonEdit, CallbackData: suggestion.Callback(suggestion.ActionEdit, sug.ID)},
			{Text: m.ButtonWithdraw, CallbackData: suggestion.Callback(suggestion.ActionWithdraw, sug.ID)},
		}},
	}
}

func moderationKeyboard(id int64, m config.ModerationCard) models.InlineKeyboardMarkup {
	return models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{{
			{Text: m.ButtonApprove, CallbackData: suggestion.Callback(suggestion.ActionApprove, id)},
			{Text: m.ButtonDecline, CallbackData: suggestion.Callback(suggestion.ActionDecline, id)},
		}},
	}
}

// confirmKeyboard is the second step of approving: the palette changes so that a
// stray tap on the first button cannot publish anything.
func confirmKeyboard(id int64, m config.ModerationCard) models.InlineKeyboardMarkup {
	return models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{{
			{Text: m.ButtonConfirm, CallbackData: suggestion.Callback(suggestion.ActionConfirm, id)},
			{Text: m.ButtonCancel, CallbackData: suggestion.Callback(suggestion.ActionAbort, id)},
		}},
	}
}

// suggestionErrors maps this flow's refusals onto what a person is told.
func suggestionMessage(m config.Messages, err error) (string, bool) {
	switch {
	case errors.Is(err, suggestion.ErrEmptyPost):
		return m.Errors.PostEmpty, true
	case errors.Is(err, suggestion.ErrTooLong):
		return m.Errors.PostTooLong, true
	case errors.Is(err, suggestion.ErrNotYours):
		return m.Errors.PostNotYours, true
	case errors.Is(err, suggestion.ErrNotPending):
		return m.Errors.PostNotPending, true
	case errors.Is(err, suggestion.ErrTooManyPending):
		return m.Errors.PostLimit, true
	case errors.Is(err, suggestion.ErrNoDraft), errors.Is(err, suggestion.ErrNotFound):
		return m.Errors.NoPostDraft, true
	default:
		return "", false
	}
}
