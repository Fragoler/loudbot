package comment

import (
	"fmt"
	"html"
	"strconv"
	"strings"
)

const (
	// startPrefix marks a /start payload that opens a comment draft, replyPrefix
	// one that answers an existing comment.
	startPrefix = "comment_"
	replyPrefix = "reply_"
	// nicknamePrefix namespaces the inline callback data of a mask button.
	nicknamePrefix = "nick:"
	// CancelCallback is the callback data of the button that drops a staged message.
	CancelCallback = "cancel"

	// maxPayloadLen is the Telegram limit for a /start payload.
	maxPayloadLen = 64
	// MaxCallbackLen is the Telegram limit for callback_data. A nickname label
	// travels inside it, so config validation checks every label against it.
	MaxCallbackLen = 64
)

// DeepLink builds the URL behind the "comment anonymously" button under a post.
func DeepLink(botUsername string, channelMessageID int) string {
	return fmt.Sprintf("https://t.me/%s?start=%s%d", botName(botUsername), startPrefix, channelMessageID)
}

// ReplyDeepLink builds the URL behind the "ответить" link under a published
// comment. The comment id travels in it, which is why a comment row is created
// before the message is sent.
func ReplyDeepLink(botUsername string, commentID int64) string {
	return fmt.Sprintf("https://t.me/%s?start=%s%d", botName(botUsername), replyPrefix, commentID)
}

func botName(botUsername string) string {
	return strings.TrimPrefix(strings.TrimSpace(botUsername), "@")
}

// StartTarget is what a /start deep link points at: a post to comment on, or a
// comment to answer.
type StartTarget struct {
	PostID    int
	CommentID int64
}

// IsReply reports whether the author arrived to answer a comment.
func (t StartTarget) IsReply() bool {
	return t.CommentID != 0
}

// ParseStartPayload reads the target out of a /start payload.
func ParseStartPayload(payload string) (StartTarget, error) {
	payload = strings.TrimSpace(payload)
	if len(payload) > maxPayloadLen {
		return StartTarget{}, fmt.Errorf("%w: %d bytes", ErrBadPayload, len(payload))
	}

	if raw, ok := strings.CutPrefix(payload, replyPrefix); ok {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return StartTarget{}, fmt.Errorf("%w: %q", ErrBadPayload, payload)
		}

		return StartTarget{CommentID: id}, nil
	}

	raw, ok := strings.CutPrefix(payload, startPrefix)
	if !ok {
		return StartTarget{}, fmt.Errorf("%w: %q", ErrBadPayload, payload)
	}

	id, err := strconv.Atoi(raw)
	if err != nil || id <= 0 {
		return StartTarget{}, fmt.Errorf("%w: %q", ErrBadPayload, payload)
	}

	return StartTarget{PostID: id}, nil
}

// NicknameCallback builds the callback data of a nickname button. The label
// itself travels in it rather than a position in the list, so editing the config
// can never make an open keyboard select the wrong mask.
func NicknameCallback(label string) string {
	return nicknamePrefix + label
}

// NicknameCallbackFits reports whether a label survives the callback_data limit.
func NicknameCallbackFits(label string) bool {
	return len(NicknameCallback(label)) <= MaxCallbackLen
}

// ParseNicknameCallback reads a nickname label back from callback data.
func ParseNicknameCallback(data string) (string, error) {
	raw, ok := strings.CutPrefix(data, nicknamePrefix)
	if !ok || strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("%w: %q", ErrBadPayload, data)
	}

	return raw, nil
}

// ReplyLink is the anchor appended to a published comment so readers can answer
// it. Its wording comes from config; a zero value leaves the comment unlinked.
type ReplyLink struct {
	URL  string
	Text string
}

func (l ReplyLink) empty() bool {
	return l.URL == "" || strings.TrimSpace(l.Text) == ""
}

// FormatBody renders a comment for the discussion group: the mask in bold, a blank
// line, the author's words, and the link that opens the bot to answer this comment.
// The result is Telegram HTML, so every part written by a person is escaped — a
// comment containing "<b>" must read as text, not as markup.
func FormatBody(nickname Nickname, text string, reply ReplyLink) string {
	parts := []string{"<b>" + html.EscapeString(strings.TrimSpace(nickname.Label)) + "</b>"}

	if text = strings.TrimSpace(text); text != "" {
		parts = append(parts, html.EscapeString(text))
	}

	if !reply.empty() {
		parts = append(parts,
			`<a href="`+html.EscapeString(reply.URL)+`">`+html.EscapeString(reply.Text)+`</a>`)
	}

	return strings.Join(parts, "\n\n")
}

// PostLink builds a link a reader can follow back to the post itself. A public
// channel is addressed by its @username; a private one by the /c/ form, which
// takes the channel id with the -100 supergroup prefix stripped.
func PostLink(post Post, channelID int64) string {
	if name := strings.TrimPrefix(strings.TrimSpace(post.ChannelUsername), "@"); name != "" {
		return fmt.Sprintf("https://t.me/%s/%d", name, post.ChannelMessageID)
	}

	internal := strconv.FormatInt(channelID, 10)
	internal = strings.TrimPrefix(internal, "-100")

	return fmt.Sprintf("https://t.me/c/%s/%d", internal, post.ChannelMessageID)
}

// ThreadLink opens the post's thread scrolled to one comment. The ?comment=
// form works for both public and /c/ post links.
func ThreadLink(post Post, channelID int64, messageID int) string {
	return fmt.Sprintf("%s?comment=%d", PostLink(post, channelID), messageID)
}
