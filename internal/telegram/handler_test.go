package telegram

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"loudbot/internal/achievement"
	"loudbot/internal/comment"
	"loudbot/internal/config"
	"loudbot/internal/profile"
	"loudbot/internal/suggestion"
)

func TestStartPayload(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		text   string
		want   string
		wantOk bool
	}{
		{name: "with payload", text: "/start comment_42", want: "comment_42", wantOk: true},
		{name: "bare start", text: "/start", want: "", wantOk: true},
		{name: "trailing spaces", text: "  /start  comment_7 ", want: "comment_7", wantOk: true},
		{name: "plain text", text: "привет", wantOk: false},
		{name: "start inside a word", text: "/startup", wantOk: false},
		{name: "empty", text: "", wantOk: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := startPayload(tc.text)
			assert.Equal(t, tc.wantOk, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestNicknameKeyboard(t *testing.T) {
	t.Parallel()

	nicknames := []comment.Nickname{{Label: "Лис"}, {Label: "Сова"}, {Label: "Ёж"}}

	markup := nicknameKeyboard(nicknames, "Отмена")

	require.Len(t, markup.InlineKeyboard, 3, "two per row, then the odd one, then cancel")
	assert.Equal(t, "Лис", markup.InlineKeyboard[0][0].Text)
	assert.Equal(t, "Сова", markup.InlineKeyboard[0][1].Text)
	assert.Equal(t, "Ёж", markup.InlineKeyboard[1][0].Text)

	cancel := markup.InlineKeyboard[2]
	require.Len(t, cancel, 1, "cancel gets a row of its own")
	assert.Equal(t, "Отмена", cancel[0].Text)
	assert.Equal(t, comment.CancelCallback, cancel[0].CallbackData)

	label, err := comment.ParseNicknameCallback(markup.InlineKeyboard[1][0].CallbackData)
	require.NoError(t, err)
	assert.Equal(t, "Ёж", label)

	for _, row := range markup.InlineKeyboard {
		for _, btn := range row {
			assert.LessOrEqual(t, len(btn.CallbackData), comment.MaxCallbackLen,
				"Telegram drops a button whose callback_data overflows")
		}
	}
}

func TestNicknameKeyboardWithoutMasks(t *testing.T) {
	t.Parallel()

	markup := nicknameKeyboard(nil, "Отмена")
	require.Len(t, markup.InlineKeyboard, 1, "cancel is always offered")
	assert.Equal(t, "Отмена", markup.InlineKeyboard[0][0].Text)
}

func TestPromptText(t *testing.T) {
	t.Parallel()

	msgs := config.DefaultMessages()

	assert.Equal(t, msgs.Prompt, promptText("  ", msgs.Prompt), "a post with no text is not quoted at all")

	quoted := promptText("Текст поста", msgs.Prompt)
	assert.Contains(t, quoted, "<blockquote>Текст поста</blockquote>")
	assert.Contains(t, quoted, msgs.Prompt)

	// The quote is sent as HTML, so a post containing markup must not break it.
	assert.Contains(t, promptText("<b>жирный</b>", msgs.Prompt), "&lt;b&gt;жирный&lt;/b&gt;")

	long := promptText(strings.Repeat("я", quoteLimit+50), msgs.Prompt)
	assert.Contains(t, long, "…", "a long post is cut down to a hint")
	assert.Less(t, utf8.RuneCountInString(long), quoteLimit+len([]rune(msgs.Prompt))+40)
}

func TestStartPrompt(t *testing.T) {
	t.Parallel()

	msgs := config.DefaultMessages()

	onPost := startPrompt(comment.StartResult{Post: comment.Post{Body: "Текст поста"}}, msgs)
	assert.Contains(t, onPost, "<blockquote>Текст поста</blockquote>")
	assert.Contains(t, onPost, msgs.Prompt)

	onComment := startPrompt(comment.StartResult{
		Post:    comment.Post{Body: "Текст поста"},
		ReplyTo: comment.Comment{ID: 7, Nickname: "Сова", Text: "а где продолжение?"},
	}, msgs)
	assert.Contains(t, onComment, "<blockquote>Сова: а где продолжение?</blockquote>",
		"a reply quotes the comment it answers, not the post")
	assert.Contains(t, onComment, msgs.ReplyPrompt)
	assert.NotContains(t, onComment, "Текст поста")
}

func TestMessageBody(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "текст", messageBody(&models.Message{Text: "текст"}))
	assert.Equal(t, "подпись", messageBody(&models.Message{Caption: "подпись"}))
	assert.Empty(t, messageBody(&models.Message{}))
}

func TestExtractMedia(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		msg     *models.Message
		want    []comment.Media
		wantErr bool
	}{
		{
			name: "text only",
			msg:  &models.Message{Text: "привет"},
			want: nil,
		},
		{
			name: "photo takes the largest size",
			msg: &models.Message{Photo: []models.PhotoSize{
				{FileID: "small", FileUniqueID: "us"},
				{FileID: "large", FileUniqueID: "ul"},
			}},
			want: []comment.Media{{Type: comment.MediaPhoto, FileID: "large", FileUniqueID: "ul"}},
		},
		{
			name: "video",
			msg:  &models.Message{Video: &models.Video{FileID: "v", FileUniqueID: "uv"}},
			want: []comment.Media{{Type: comment.MediaVideo, FileID: "v", FileUniqueID: "uv"}},
		},
		{
			name: "document",
			msg:  &models.Message{Document: &models.Document{FileID: "d", FileUniqueID: "ud"}},
			want: []comment.Media{{Type: comment.MediaDocument, FileID: "d", FileUniqueID: "ud"}},
		},
		{
			name:    "sticker is refused",
			msg:     &models.Message{Sticker: &models.Sticker{FileID: "s"}},
			wantErr: true,
		},
		{
			name:    "voice is refused",
			msg:     &models.Message{Voice: &models.Voice{FileID: "vo"}},
			wantErr: true,
		},
		{
			name: "empty message carries nothing",
			msg:  &models.Message{},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := extractMedia(tc.msg)
			if tc.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestUserMessage(t *testing.T) {
	t.Parallel()

	msgs := config.DefaultMessages()

	cases := []struct {
		name string
		err  error
		want string
	}{
		{name: "guard reason reaches the author", err: &comment.RejectedError{Reason: "слишком часто"}, want: "слишком часто"},
		{name: "nothing staged", err: comment.ErrNothingStaged, want: msgs.Errors.NothingStaged},
		{name: "deleted parent comment", err: comment.ErrUnknownComment, want: msgs.Errors.UnknownComment},
		{name: "banned", err: comment.ErrBanned, want: msgs.Errors.Banned},
		{name: "wrapped sentinel", err: errors.Join(errors.New("ctx"), comment.ErrDraftExpired), want: msgs.Errors.DraftExpired},
		{name: "no draft", err: comment.ErrNoDraft, want: msgs.Errors.NoDraft},
		{name: "internal errors stay opaque", err: errors.New("pq: connection refused"), want: msgs.Errors.Internal},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, userMessage(msgs, tc.err))
		})
	}
}

func TestExpected(t *testing.T) {
	t.Parallel()

	assert.True(t, expected(comment.ErrEmptyComment))
	assert.True(t, expected(comment.ErrNothingStaged))
	assert.True(t, expected(comment.ErrUnknownComment))
	assert.True(t, expected(&comment.RejectedError{Reason: "стоп-слово"}))
	assert.False(t, expected(errors.New("boom")), "a real failure must still be logged as an error")
}

func TestProfileText(t *testing.T) {
	t.Parallel()

	m := config.DefaultMessages().Profile

	got := profileText(profile.Profile{
		Activity: profile.Activity{Comments: 12, Replies: 4, Posts: 3, PostsPending: 2},
		Achievements: []profile.Achievement{
			{Title: "Полуночник", Description: "Длинный комментарий ночью"},
			{Title: "Крикун"},
		},
		Nicknames: []comment.Nickname{{Label: "Лис"}, {Label: "Сова"}},
	}, m)

	assert.Contains(t, got, "<b>📊 Ваша статистика</b>")
	assert.Contains(t, got, "💬 Комментариев: 12")
	assert.Contains(t, got, "↩️ Ответов: 4")
	assert.Contains(t, got, "📣 Постов опубликовано: 3")
	assert.Contains(t, got, "📝 На рассмотрении: 2")
	assert.Contains(t, got, "<b>🏅 Достижения (2)</b>", "a heading says how long its list is")
	assert.Contains(t, got, "• <b>Полуночник</b>")
	assert.Contains(t, got, "<i>Длинный комментарий ночью</i>")
	assert.Contains(t, got, "• <b>Крикун</b>")
	assert.NotContains(t, got, "Крикун</b>\n   <i>", "an achievement with no description gets no empty line")
	assert.Contains(t, got, "<b>🎭 Доступные псевдонимы (2)</b>")
	assert.Contains(t, got, "• Лис")
	assert.False(t, strings.HasSuffix(got, "\n"), "no trailing blank line")
}

func TestProfileTextWithNothingEarned(t *testing.T) {
	t.Parallel()

	m := config.DefaultMessages().Profile

	got := profileText(profile.Profile{Nicknames: []comment.Nickname{{Label: "Лис"}}}, m)

	assert.Contains(t, got, "📣 Постов опубликовано: 0")
	assert.NotContains(t, got, "На рассмотрении", "the in-flight line stays out when nothing is in flight")
	assert.Contains(t, got, "<b>🏅 Достижения</b>", "an empty list drops the count")
	assert.NotContains(t, got, "Достижения (0)")
	assert.Contains(t, got, "<i>"+m.NoAchievements+"</i>")
}

func TestProfileTextEscapesWhatItDidNotWrite(t *testing.T) {
	t.Parallel()

	// Titles and masks come from the database; raw markup there must not reach
	// Telegram as markup, or the whole message fails to parse.
	got := profileText(profile.Profile{
		Achievements: []profile.Achievement{{Title: "<b>взлом", Description: "a & b"}},
		Nicknames:    []comment.Nickname{{Label: "<i>Лис"}},
	}, config.DefaultMessages().Profile)

	assert.Contains(t, got, "&lt;b&gt;взлом")
	assert.Contains(t, got, "a &amp; b")
	assert.Contains(t, got, "&lt;i&gt;Лис")
}

func TestAchievementText(t *testing.T) {
	t.Parallel()

	head := config.DefaultMessages().Achievement

	withDescription := achievementText(head, achievement.Granted{
		Title:       "Полуночник",
		Description: "Длинный комментарий ночью",
	})
	assert.Equal(t,
		"<b>🏅 Новое достижение</b>\n\n<b>Полуночник</b>\n<i>Длинный комментарий ночью</i>",
		withDescription)

	bare := achievementText(head, achievement.Granted{Title: "Крикун"})
	assert.Equal(t, "<b>🏅 Новое достижение</b>\n\n<b>Крикун</b>", bare)

	escaped := achievementText(head, achievement.Granted{Title: "<b>взлом"})
	assert.Contains(t, escaped, "&lt;b&gt;взлом")
}

func TestModerationCard(t *testing.T) {
	t.Parallel()

	m := config.DefaultMessages().ModerationCard

	fresh := moderationCard(suggestion.Suggestion{ID: 7, Text: "Предлагаю котика"}, m)
	assert.Equal(t, "<b>📝 Предложка #7</b>\n\nПредлагаю котика", fresh)

	edited := time.Now()
	changed := moderationCard(suggestion.Suggestion{ID: 7, Text: "Предлагаю котика", EditedAt: &edited}, m)
	assert.Contains(t, changed, "изменено автором", "a moderator must see that the text moved under them")

	// The post is written by a reader, so it cannot carry markup into the card.
	escaped := moderationCard(suggestion.Suggestion{ID: 7, Text: "<b>взлом"}, m)
	assert.Contains(t, escaped, "&lt;b&gt;взлом")

	empty := moderationCard(suggestion.Suggestion{ID: 7}, m)
	assert.Contains(t, empty, "—", "a media-only post still needs a body in the card")
}

func TestModerationKeyboardConfirmsBeforePublishing(t *testing.T) {
	t.Parallel()

	m := config.DefaultMessages().ModerationCard

	first := moderationKeyboard(7, m).InlineKeyboard
	require.Len(t, first, 1)
	require.Len(t, first[0], 2)
	assert.Equal(t, m.ButtonApprove, first[0][0].Text)
	assert.Equal(t, m.ButtonDecline, first[0][1].Text)

	action, id, ok := suggestion.ParseCallback(first[0][0].CallbackData)
	require.True(t, ok)
	assert.Equal(t, suggestion.ActionApprove, action, "the first tap only asks")
	assert.Equal(t, int64(7), id)

	// Declining is one tap: it publishes nothing and is not the dangerous one.
	action, _, ok = suggestion.ParseCallback(first[0][1].CallbackData)
	require.True(t, ok)
	assert.Equal(t, suggestion.ActionDecline, action)

	second := confirmKeyboard(7, m).InlineKeyboard
	require.Len(t, second, 1)
	require.Len(t, second[0], 2)
	assert.Equal(t, m.ButtonConfirm, second[0][0].Text)
	assert.Equal(t, m.ButtonCancel, second[0][1].Text)

	action, _, ok = suggestion.ParseCallback(second[0][0].CallbackData)
	require.True(t, ok)
	assert.Equal(t, suggestion.ActionConfirm, action, "only the second tap publishes")

	action, _, ok = suggestion.ParseCallback(second[0][1].CallbackData)
	require.True(t, ok)
	assert.Equal(t, suggestion.ActionAbort, action, "cancelling puts the first palette back")
}

func TestPublishedNotice(t *testing.T) {
	t.Parallel()

	m := config.DefaultMessages().Posts

	withLink := publishedNotice(m, "https://t.me/my_channel/42")
	assert.Contains(t, withLink, esc(m.Approved))
	assert.Contains(t, withLink, `<a href="https://t.me/my_channel/42">`)
	assert.Contains(t, withLink, esc(m.OpenPost))

	// A post with no link still gets the news.
	assert.Equal(t, esc(m.Approved), publishedNotice(m, ""))
}

func TestAuthorKeyboard(t *testing.T) {
	t.Parallel()

	m := config.DefaultMessages().Posts

	rows := authorKeyboard(suggestion.Suggestion{ID: 7}, m).InlineKeyboard
	require.Len(t, rows, 1)
	require.Len(t, rows[0], 2)

	edit, _, ok := suggestion.ParseCallback(rows[0][0].CallbackData)
	require.True(t, ok)
	assert.Equal(t, suggestion.ActionEdit, edit)

	drop, _, ok := suggestion.ParseCallback(rows[0][1].CallbackData)
	require.True(t, ok)
	assert.Equal(t, suggestion.ActionWithdraw, drop)
}

func TestSuggestionMessagesAreMapped(t *testing.T) {
	t.Parallel()

	m := config.DefaultMessages()

	cases := []struct {
		err  error
		want string
	}{
		{err: suggestion.ErrEmptyPost, want: m.Errors.PostEmpty},
		{err: suggestion.ErrTooLong, want: m.Errors.PostTooLong},
		{err: suggestion.ErrNotYours, want: m.Errors.PostNotYours},
		{err: suggestion.ErrNotPending, want: m.Errors.PostNotPending},
		{err: suggestion.ErrTooManyPending, want: m.Errors.PostLimit},
		{err: suggestion.ErrNoDraft, want: m.Errors.NoPostDraft},
		{err: suggestion.ErrNotFound, want: m.Errors.NoPostDraft},
	}

	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, userMessage(m, tc.err))
			assert.True(t, expected(tc.err), "these are outcomes of user input, not failures")
		})
	}
}

func TestReplyNoticeText(t *testing.T) {
	t.Parallel()

	head := config.DefaultMessages().ReplyNotice

	got := replyNoticeText(head, comment.ReplyNotice{Nickname: "Сова", Text: "а где продолжение?"})
	assert.Equal(t, "<b>💬 Вам ответили</b>\n\n<b>Сова</b>\nа где продолжение?", got)

	mediaOnly := replyNoticeText(head, comment.ReplyNotice{Nickname: "Сова"})
	assert.Equal(t, "<b>💬 Вам ответили</b>\n\n<b>Сова</b>", mediaOnly)

	escaped := replyNoticeText(head, comment.ReplyNotice{Nickname: "<i>Сова", Text: "<b>взлом"})
	assert.Contains(t, escaped, "&lt;i&gt;Сова")
	assert.Contains(t, escaped, "&lt;b&gt;взлом")
}
