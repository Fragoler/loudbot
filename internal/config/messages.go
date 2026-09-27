package config

import (
	"fmt"
	"sort"
	"strings"
)

// Messages is every line the bot shows a person. Defaults live in DefaultMessages,
// and go-toml leaves absent keys untouched, so an operator overrides only the
// wording they actually want to change.
type Messages struct {
	ButtonComment  string `toml:"button_comment"`
	ButtonOpenPost string `toml:"button_open_post"`
	ButtonCancel   string `toml:"button_cancel"`
	// ReplyLink is the text of the link under every published comment.
	ReplyLink string `toml:"reply_link"`

	// Invite is the bot's own first comment under every post.
	Invite         string `toml:"invite"`
	Help           string `toml:"help"`
	Prompt         string `toml:"prompt"`
	ReplyPrompt    string `toml:"reply_prompt"`
	ChooseNickname string `toml:"choose_nickname"`
	Published      string `toml:"published"`
	// Achievement is shown when a comment earns one; it takes the title.
	Achievement string `toml:"achievement"`
	// ReplyNotice heads the message a comment's author gets when someone answers
	// it; the reply's mask and words follow, with a button to the reply.
	ReplyNotice       string `toml:"reply_notice"`
	ButtonOpenComment string `toml:"button_open_comment"`
	Cancelled         string `toml:"cancelled"`

	// Profile is the /profile screen. Counters and lists are filled into it.
	Profile Profile `toml:"profile"`
	// Posts is the suggestion flow as its author sees it.
	Posts Posts `toml:"posts"`
	// ModerationCard is what moderators act on.
	ModerationCard ModerationCard `toml:"moderation"`

	Errors Errors `toml:"errors"`
}

// Profile is the wording of the /profile screen. These are plain text: the bot
// adds the bold and italic markup itself and escapes every line, so an operator
// can put emoji here without worrying about breaking Telegram's HTML.
type Profile struct {
	Title            string `toml:"title"`
	Comments         string `toml:"comments"`
	Replies          string `toml:"replies"`
	Posts            string `toml:"posts"`
	PostsPending     string `toml:"posts_pending"`
	AchievementsHead string `toml:"achievements_head"`
	NoAchievements   string `toml:"no_achievements"`
	NicknamesHead    string `toml:"nicknames_head"`
}

// Posts is the wording of the suggestion flow: plain text, the bot adds markup.
type Posts struct {
	ButtonEdit     string `toml:"button_edit"`
	ButtonWithdraw string `toml:"button_withdraw"`

	Prompt     string `toml:"prompt"`
	EditPrompt string `toml:"edit_prompt"`
	Sent       string `toml:"sent"`
	Edited     string `toml:"edited"`
	Withdrawn  string `toml:"withdrawn"`

	PendingHead    string `toml:"pending_head"`
	PendingEmpty   string `toml:"pending_empty"`
	PublishedHead  string `toml:"published_head"`
	PublishedEmpty string `toml:"published_empty"`
	// OpenPost labels the link to a published post.
	OpenPost string `toml:"open_post"`
	// Approved is what the author is told once the post actually goes out, and
	// Declined when it is refused.
	Approved string `toml:"approved"`
	Declined string `toml:"declined"`
}

// ModerationCard is the wording of the moderators' card. Named apart from the
// [moderation] section, which holds the chat id rather than any copy.
type ModerationCard struct {
	Head   string `toml:"head"`
	Edited string `toml:"edited"`

	ButtonApprove string `toml:"button_approve"`
	ButtonDecline string `toml:"button_decline"`
	ButtonConfirm string `toml:"button_confirm"`
	ButtonCancel  string `toml:"button_cancel"`

	Confirm string `toml:"confirm"`
	// Published is stamped on the card once the post is in the channel.
	Published string `toml:"published"`
	Declined  string `toml:"declined"`
	Withdrawn string `toml:"withdrawn"`
	Failed    string `toml:"failed"`
}

// Errors is what an author is told when the bot refuses their message. Nothing
// here may leak internal detail: an unmapped failure shows Internal.
type Errors struct {
	Banned         string `toml:"banned"`
	BadPayload     string `toml:"bad_payload"`
	UnknownPost    string `toml:"unknown_post"`
	UnknownComment string `toml:"unknown_comment"`
	NoNicknames    string `toml:"no_nicknames"`
	NoDraft        string `toml:"no_draft"`
	DraftExpired   string `toml:"draft_expired"`
	Nickname       string `toml:"nickname"`
	Empty          string `toml:"empty"`
	TooLong        string `toml:"too_long"`
	NothingStaged  string `toml:"nothing_staged"`
	PostEmpty      string `toml:"post_empty"`
	PostTooLong    string `toml:"post_too_long"`
	PostNotYours   string `toml:"post_not_yours"`
	PostNotPending string `toml:"post_not_pending"`
	PostLimit      string `toml:"post_limit"`
	NoPostDraft    string `toml:"no_post_draft"`
	Unsupported    string `toml:"unsupported"`
	Internal       string `toml:"internal"`
}

// DefaultMessages is the built-in Russian copy. Tests build a config from it, so
// there is exactly one place where the wording lives.
func DefaultMessages() Messages {
	return Messages{
		ButtonComment:  "💬 Комментировать анонимно",
		ButtonOpenPost: "Открыть пост",
		ButtonCancel:   "Отмена",
		ReplyLink:      "ответить",

		Invite: "Комментарии к этому посту анонимные. Нажмите кнопку ниже — и пишите.",
		Help: "Этот бот публикует анонимные комментарии и принимает посты.\n\n" +
			"Нажмите «💬 Комментировать анонимно» под постом в канале — " +
			"и напишите сюда текст или пришлите медиа.\n\n" +
			"/suggest — предложить пост\n" +
			"/pending — предложки на рассмотрении\n" +
			"/posts — ваши опубликованные посты\n" +
			"/profile — ваша статистика",
		Prompt:         "Напишите комментарий к посту — текстом или медиа.",
		ReplyPrompt:    "Напишите ответ — текстом или медиа.",
		ChooseNickname: "Под каким псевдонимом отправить?",
		Published:      "Комментарий отправлен.",
		Achievement:    "🏅 Новое достижение",

		ReplyNotice:       "💬 Вам ответили",
		ButtonOpenComment: "Открыть комментарий",
		Cancelled:         "Черновик удалён. Напишите новый комментарий.",

		Profile: Profile{
			Title:            "📊 Ваша статистика",
			Comments:         "💬 Комментариев: %d",
			Replies:          "↩️ Ответов: %d",
			Posts:            "📣 Постов опубликовано: %d",
			PostsPending:     "📝 На рассмотрении: %d",
			AchievementsHead: "🏅 Достижения",
			NoAchievements:   "Пока ни одного — они открывают новые псевдонимы.",
			NicknamesHead:    "🎭 Доступные псевдонимы",
		},

		Posts: Posts{
			ButtonEdit:     "✏️ Редактировать",
			ButtonWithdraw: "🗑 Отменить",

			Prompt:     "Пришлите текст поста — можно с медиа.",
			EditPrompt: "Пришлите новый текст поста.",
			Sent:       "Пост отправлен на рассмотрение.",
			Edited:     "Пост обновлён — модераторы видят новый текст.",
			Withdrawn:  "Предложка отменена.",

			PendingHead:    "🕓 На рассмотрении",
			PendingEmpty:   "Пока ничего не ждёт решения.",
			PublishedHead:  "📣 Опубликованные посты",
			PublishedEmpty: "Пока ни один ваш пост не вышел.",
			OpenPost:       "Открыть пост",
			Approved:       "✅ Ваш пост опубликован.",
			Declined:       "❌ Ваш пост отклонён.",
		},

		ModerationCard: ModerationCard{
			Head:   "📝 Предложка",
			Edited: "изменено автором",

			ButtonApprove: "✅ Одобрить",
			ButtonDecline: "❌ Отклонить",
			ButtonConfirm: "Одобрить",
			ButtonCancel:  "Отменить",

			Confirm:   "Опубликовать пост в канал?",
			Published: "✅ Опубликовано",
			Declined:  "❌ Отклонено",
			Withdrawn: "🗑 Отменено автором",
			Failed:    "⚠️ Не удалось опубликовать",
		},

		Errors: Errors{
			Banned:         "Вы не можете оставлять комментарии.",
			BadPayload:     "Ссылка не распознана. Откройте её кнопкой под постом.",
			UnknownPost:    "Пост не найден — возможно, он удалён.",
			UnknownComment: "Комментарий не найден — возможно, он удалён.",
			NoNicknames:    "Список псевдонимов пуст, комментарии временно недоступны.",
			NoDraft:        "Сначала нажмите «💬 Комментировать анонимно» под нужным постом.",
			DraftExpired:   "Время на комментарий истекло. Нажмите кнопку под постом ещё раз.",
			Nickname:       "Этот псевдоним больше недоступен, выберите другой.",
			Empty:          "Пустой комментарий: пришлите текст или медиа.",
			TooLong:        "Комментарий слишком длинный, сократите текст.",
			NothingStaged:  "Нечего отправлять — сначала напишите комментарий.",
			PostEmpty:      "Пустой пост: пришлите текст или медиа.",
			PostTooLong:    "Текст поста слишком длинный, сократите его.",
			PostNotYours:   "Это чужая предложка.",
			PostNotPending: "Решение по этой предложке уже принято.",
			PostLimit:      "Слишком много предложек ждёт решения. Дождитесь ответа по предыдущим.",
			NoPostDraft:    "Сначала начните предложку командой /suggest.",
			Unsupported:    "Такой тип вложения не поддерживается.",
			Internal:       "Что-то пошло не так, попробуйте ещё раз позже.",
		},
	}
}

// validate rejects a blank line: an empty button or link would render as an
// unusable control rather than fall back to the default.
func (m Messages) validate() []error {
	named := map[string]string{
		"messages.button_comment":            m.ButtonComment,
		"messages.button_open_post":          m.ButtonOpenPost,
		"messages.button_cancel":             m.ButtonCancel,
		"messages.reply_link":                m.ReplyLink,
		"messages.invite":                    m.Invite,
		"messages.help":                      m.Help,
		"messages.prompt":                    m.Prompt,
		"messages.reply_prompt":              m.ReplyPrompt,
		"messages.choose_nickname":           m.ChooseNickname,
		"messages.published":                 m.Published,
		"messages.achievement":               m.Achievement,
		"messages.reply_notice":              m.ReplyNotice,
		"messages.button_open_comment":       m.ButtonOpenComment,
		"messages.cancelled":                 m.Cancelled,
		"messages.profile.title":             m.Profile.Title,
		"messages.profile.comments":          m.Profile.Comments,
		"messages.profile.replies":           m.Profile.Replies,
		"messages.profile.posts":             m.Profile.Posts,
		"messages.profile.posts_pending":     m.Profile.PostsPending,
		"messages.profile.achievements_head": m.Profile.AchievementsHead,
		"messages.profile.no_achievements":   m.Profile.NoAchievements,
		"messages.profile.nicknames_head":    m.Profile.NicknamesHead,
		"messages.errors.banned":             m.Errors.Banned,
		"messages.errors.bad_payload":        m.Errors.BadPayload,
		"messages.errors.unknown_post":       m.Errors.UnknownPost,
		"messages.errors.unknown_comment":    m.Errors.UnknownComment,
		"messages.errors.no_nicknames":       m.Errors.NoNicknames,
		"messages.errors.no_draft":           m.Errors.NoDraft,
		"messages.errors.draft_expired":      m.Errors.DraftExpired,
		"messages.errors.nickname":           m.Errors.Nickname,
		"messages.errors.empty":              m.Errors.Empty,
		"messages.errors.too_long":           m.Errors.TooLong,
		"messages.errors.nothing_staged":     m.Errors.NothingStaged,
		"messages.errors.unsupported":        m.Errors.Unsupported,
		"messages.errors.internal":           m.Errors.Internal,
	}

	var errs []error
	for _, key := range sortedKeys(named) {
		if strings.TrimSpace(named[key]) == "" {
			errs = append(errs, fmt.Errorf("%s is empty", key))
		}
	}

	return errs
}

// sortedKeys keeps the reported errors in a stable order.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	return keys
}
