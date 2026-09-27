package comment_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"loudbot/internal/comment"
)

func TestDeepLink(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "https://t.me/anon_bot?start=comment_42", comment.DeepLink("anon_bot", 42))
	assert.Equal(t, "https://t.me/anon_bot?start=comment_42", comment.DeepLink(" @anon_bot ", 42))
}

func TestParseStartPayload(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		payload string
		want    comment.StartTarget
		wantErr bool
	}{
		{name: "comment on a post", payload: "comment_42", want: comment.StartTarget{PostID: 42}},
		{name: "trims spaces", payload: "  comment_7  ", want: comment.StartTarget{PostID: 7}},
		{name: "reply to a comment", payload: "reply_9", want: comment.StartTarget{CommentID: 9}},
		{name: "wrong prefix", payload: "suggest_42", wantErr: true},
		{name: "empty", payload: "", wantErr: true},
		{name: "prefix only", payload: "comment_", wantErr: true},
		{name: "reply prefix only", payload: "reply_", wantErr: true},
		{name: "not a number", payload: "comment_abc", wantErr: true},
		{name: "reply not a number", payload: "reply_abc", wantErr: true},
		{name: "zero", payload: "comment_0", wantErr: true},
		{name: "reply zero", payload: "reply_0", wantErr: true},
		{name: "negative", payload: "comment_-1", wantErr: true},
		{name: "too long", payload: "comment_" + strings.Repeat("9", 60), wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := comment.ParseStartPayload(tc.payload)
			if tc.wantErr {
				require.ErrorIs(t, err, comment.ErrBadPayload)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.want.CommentID != 0, got.IsReply())
		})
	}
}

func TestStartPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	const bot = "anon_bot"

	post, err := comment.ParseStartPayload(payloadOf(comment.DeepLink(bot, 123)))
	require.NoError(t, err)
	assert.Equal(t, comment.StartTarget{PostID: 123}, post)

	reply, err := comment.ParseStartPayload(payloadOf(comment.ReplyDeepLink(bot, 456)))
	require.NoError(t, err)
	assert.Equal(t, comment.StartTarget{CommentID: 456}, reply)
}

// payloadOf extracts what Telegram hands the bot as the /start argument.
func payloadOf(deepLink string) string {
	_, payload, _ := strings.Cut(deepLink, "?start=")

	return payload
}

func TestCallbackRoundTrip(t *testing.T) {
	t.Parallel()

	// The label travels verbatim, so a config edit cannot shift what a button means.
	for _, label := range []string{"Лис", "Сова", "Ёж", "a b c", "nick:weird"} {
		got, err := comment.ParseNicknameCallback(comment.NicknameCallback(label))
		require.NoError(t, err, label)
		assert.Equal(t, label, got)
	}
}

func TestNicknameCallbackFits(t *testing.T) {
	t.Parallel()

	// "nick:" is 5 bytes of the 64 Telegram allows for callback_data.
	assert.True(t, comment.NicknameCallbackFits("Тушканчик"))
	assert.True(t, comment.NicknameCallbackFits(strings.Repeat("я", 29)+"a"), "59 bytes is the longest that fits")
	assert.False(t, comment.NicknameCallbackFits(strings.Repeat("я", 30)), "60 bytes overflows")
	assert.LessOrEqual(t, len(comment.NicknameCallback(strings.Repeat("я", 29)+"a")), comment.MaxCallbackLen)
}

func TestCallbackParseRejectsForeignData(t *testing.T) {
	t.Parallel()

	// Cancel travels as its own fixed value and must not parse as a mask.
	_, err := comment.ParseNicknameCallback(comment.CancelCallback)
	require.ErrorIs(t, err, comment.ErrBadPayload)

	for _, data := range []string{"", "nick:", "nick:   ", "Лис"} {
		_, err := comment.ParseNicknameCallback(data)
		require.ErrorIs(t, err, comment.ErrBadPayload, data)
	}
}

func TestFormatBody(t *testing.T) {
	t.Parallel()

	nick := comment.Nickname{Label: "Лис"}
	link := comment.ReplyLink{URL: "https://t.me/anon_bot?start=reply_7", Text: "ответить"}

	cases := []struct {
		name string
		nick comment.Nickname
		text string
		link comment.ReplyLink
		want string
	}{
		{
			name: "mask, text, then the reply link",
			nick: nick, text: "привет", link: link,
			want: "<b>Лис</b>\n\nпривет\n\n<a href=\"https://t.me/anon_bot?start=reply_7\">ответить</a>",
		},
		{
			name: "trims text",
			nick: nick, text: "  привет  ",
			want: "<b>Лис</b>\n\nпривет",
		},
		{
			name: "media only keeps the mask and the link",
			nick: nick, link: link,
			want: "<b>Лис</b>\n\n<a href=\"https://t.me/anon_bot?start=reply_7\">ответить</a>",
		},
		{
			name: "multiline text survives",
			nick: nick, text: "one\ntwo",
			want: "<b>Лис</b>\n\none\ntwo",
		},
		{
			// Telegram parses the result as HTML, so a comment cannot inject markup.
			name: "markup in the text is escaped",
			nick: nick, text: "<b>жирный</b> & <script>",
			want: "<b>Лис</b>\n\n&lt;b&gt;жирный&lt;/b&gt; &amp; &lt;script&gt;",
		},
		{
			name: "markup in the mask is escaped too",
			nick: comment.Nickname{Label: "<i>Лис"}, text: "привет",
			want: "<b>&lt;i&gt;Лис</b>\n\nпривет",
		},
		{
			// The wording comes from config, so it is escaped like any other input.
			name: "the link wording is configurable and escaped",
			nick: nick, text: "привет",
			link: comment.ReplyLink{URL: "https://t.me/b?start=reply_1", Text: "reply <here>"},
			want: "<b>Лис</b>\n\nпривет\n\n" +
				"<a href=\"https://t.me/b?start=reply_1\">reply &lt;here&gt;</a>",
		},
		{
			name: "a blank wording drops the link rather than rendering an empty one",
			nick: nick, text: "привет",
			link: comment.ReplyLink{URL: "https://t.me/b?start=reply_1", Text: "  "},
			want: "<b>Лис</b>\n\nпривет",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, comment.FormatBody(tc.nick, tc.text, tc.link))
		})
	}
}

func TestReplyDeepLink(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "https://t.me/anon_bot?start=reply_7", comment.ReplyDeepLink("anon_bot", 7))
	assert.Equal(t, "https://t.me/anon_bot?start=reply_7", comment.ReplyDeepLink(" @anon_bot ", 7))
}

func TestPostLink(t *testing.T) {
	t.Parallel()

	public := comment.Post{ChannelMessageID: 42, ChannelUsername: "anon_channel"}
	assert.Equal(t, "https://t.me/anon_channel/42", comment.PostLink(public, -1001234567890))

	withAt := comment.Post{ChannelMessageID: 42, ChannelUsername: "@anon_channel"}
	assert.Equal(t, "https://t.me/anon_channel/42", comment.PostLink(withAt, -1001234567890))

	// A private channel has no username, so the link takes the /c/ form with the
	// -100 supergroup prefix stripped off the channel id.
	private := comment.Post{ChannelMessageID: 42}
	assert.Equal(t, "https://t.me/c/1234567890/42", comment.PostLink(private, -1001234567890))
}

func TestThreadLink(t *testing.T) {
	t.Parallel()

	public := comment.Post{ChannelMessageID: 42, ChannelUsername: "anon_channel"}
	assert.Equal(t, "https://t.me/anon_channel/42?comment=9100", comment.ThreadLink(public, -1001234567890, 9100))

	private := comment.Post{ChannelMessageID: 42}
	assert.Equal(t, "https://t.me/c/1234567890/42?comment=9100", comment.ThreadLink(private, -1001234567890, 9100))
}
