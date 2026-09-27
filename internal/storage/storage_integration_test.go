//go:build integration

// These tests run the real SQL against a real Postgres. They are behind a build
// tag so that `task test` stays offline; see `task test:integration`.
package storage_test

import (
	"context"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"loudbot/internal/achievement"
	"loudbot/internal/comment"
	"loudbot/internal/config"
	"loudbot/internal/profile"
	"loudbot/internal/storage"
	"loudbot/internal/suggestion"
)

// envDSN points at a throwaway database; everything in it is dropped and rebuilt.
const envDSN = "LOUDBOT_TEST_POSTGRES"

func dsn(t *testing.T) string {
	t.Helper()

	raw := os.Getenv(envDSN)
	if raw == "" {
		t.Skipf("%s is not set", envDSN)
	}

	return raw
}

// testConfig turns the test DSN back into the struct the production code takes,
// which incidentally checks that config.Postgres can express a real connection.
func testConfig(t *testing.T) config.Postgres {
	t.Helper()

	u, err := url.Parse(dsn(t))
	require.NoError(t, err)

	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)

	password, _ := u.User.Password()

	return config.Postgres{
		Host:     u.Hostname(),
		Port:     port,
		User:     u.User.Username(),
		Password: password,
		DBName:   strings.TrimPrefix(u.Path, "/"),
		SSLMode:  u.Query().Get("sslmode"),
		MaxConns: 5,
	}
}

// probe is an independent connection used to assert on rows the repository wrote.
func probe(ctx context.Context, t *testing.T) *pgx.Conn {
	t.Helper()

	conn, err := pgx.Connect(ctx, dsn(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	return conn
}

// seed is the nicknames table as the migration leaves it, captured before any
// test touches it. Tests add and retire masks, so reset restores this snapshot
// rather than truncating — and taking it from the database rather than copying
// the migration's list keeps the two from drifting apart.
type mask struct {
	code  string
	label string
}

var (
	seedOnce sync.Once
	seed     []mask
	// fox and owl are two seeded labels, named once the snapshot is taken. These
	// tests do not run in parallel, so plain variables are enough.
	fox, owl string
)

func captureSeed(ctx context.Context, t *testing.T, conn *pgx.Conn) {
	t.Helper()

	rows, err := conn.Query(ctx, `SELECT code, label FROM nicknames ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()

	for rows.Next() {
		var m mask
		require.NoError(t, rows.Scan(&m.code, &m.label))
		seed = append(seed, m)
	}
	require.NoError(t, rows.Err())
	require.GreaterOrEqual(t, len(seed), 2, "the tests need at least two seeded masks")

	fox, owl = seed[0].label, seed[1].label
}

func restoreSeed(ctx context.Context, t *testing.T, conn *pgx.Conn) {
	t.Helper()

	_, err := conn.Exec(ctx, `DELETE FROM nicknames`)
	require.NoError(t, err)

	for _, m := range seed {
		_, err := conn.Exec(ctx, `INSERT INTO nicknames (code, label) VALUES ($1, $2)`, m.code, m.label)
		require.NoError(t, err)
	}
}

// reset empties everything the tests write. Without it a test would read rows
// left by an earlier run.
const reset = `
TRUNCATE posts, comment_drafts, comments, suggested_posts, reports, identity_map,
         audit_log, users, private_messages, user_achievements, achievement_nicknames,
         achievement_rules, achievements, suggestion_drafts, suggested_posts
RESTART IDENTITY CASCADE`

// Masks live in the nicknames table, seeded by the migration.
func open(t *testing.T) (*storage.Storage, context.Context) {
	t.Helper()

	cfg := testConfig(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	require.NoError(t, storage.Migrate(ctx, cfg, log))

	conn := probe(ctx, t)

	_, err := conn.Exec(ctx, reset)
	require.NoError(t, err)

	seedOnce.Do(func() { captureSeed(ctx, t, conn) })
	restoreSeed(ctx, t, conn)

	st, err := storage.Open(ctx, cfg, log)
	require.NoError(t, err)
	t.Cleanup(st.Close)

	return st, ctx
}

func TestMigrateIsIdempotent(t *testing.T) {
	cfg := testConfig(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	require.NoError(t, storage.Migrate(ctx, cfg, log))
	require.NoError(t, storage.Migrate(ctx, cfg, log), "a second run must be a no-op")
}

func TestUserLifecycle(t *testing.T) {
	st, ctx := open(t)

	const userID = int64(10_001)

	user, err := st.EnsureUser(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, userID, user.ID)
	assert.False(t, user.Banned)
	assert.Empty(t, user.LastNickname, "a fresh user has no remembered mask")

	again, err := st.EnsureUser(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, user, again, "EnsureUser is an upsert, not an insert")

	require.NoError(t, st.SetLastNickname(ctx, userID, fox))

	updated, err := st.EnsureUser(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, fox, updated.LastNickname)
}

func TestPostLinking(t *testing.T) {
	st, ctx := open(t)

	post := comment.Post{
		ChannelMessageID:    4242,
		DiscussionChatID:    -100500,
		DiscussionMessageID: 77,
		Body:                "Текст поста",
		ChannelUsername:     "anon_channel",
	}
	require.NoError(t, st.LinkPost(ctx, post))

	// Telegram may deliver the auto-forward more than once.
	require.NoError(t, st.LinkPost(ctx, post))

	got, err := st.Post(ctx, post.ChannelMessageID)
	require.NoError(t, err)
	assert.Equal(t, post.DiscussionChatID, got.DiscussionChatID)
	assert.Equal(t, post.DiscussionMessageID, got.DiscussionMessageID)
	assert.Zero(t, got.InviteMessageID, "a NULL invite column must read back as zero")
	assert.Equal(t, "Текст поста", got.Body, "the body is what the bot quotes back to an author")
	assert.Equal(t, "anon_channel", got.ChannelUsername)
	assert.False(t, got.CreatedAt.IsZero())

	require.NoError(t, st.MarkInvitePosted(ctx, post.ChannelMessageID, 555))

	got, err = st.Post(ctx, post.ChannelMessageID)
	require.NoError(t, err)
	assert.Equal(t, 555, got.InviteMessageID)

	// A redelivered auto-forward re-links the post; the invite marker must survive.
	require.NoError(t, st.LinkPost(ctx, post))

	got, err = st.Post(ctx, post.ChannelMessageID)
	require.NoError(t, err)
	assert.Equal(t, 555, got.InviteMessageID, "re-linking must not clear the invite marker")

	_, err = st.Post(ctx, 999_999)
	require.ErrorIs(t, err, comment.ErrNotFound)
}

func TestDraftLifecycle(t *testing.T) {
	st, ctx := open(t)

	const userID = int64(10_002)

	_, err := st.EnsureUser(ctx, userID)
	require.NoError(t, err)

	_, err = st.Draft(ctx, userID)
	require.ErrorIs(t, err, comment.ErrNotFound)

	created := time.Now().UTC().Truncate(time.Millisecond)
	draft := comment.Draft{UserID: userID, PostID: 4242, CreatedAt: created}
	require.NoError(t, st.SaveDraft(ctx, draft))

	got, err := st.Draft(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, draft.PostID, got.PostID)
	assert.False(t, got.Staged(), "a fresh draft has nothing written yet")
	assert.Empty(t, got.Media, "a NULL-free media column reads back as an empty slice")
	assert.WithinDuration(t, created, got.CreatedAt, time.Millisecond)

	// Staging a message must overwrite in place: user_id is the primary key.
	draft.ReplyToCommentID = 0
	draft.Body = "привет"
	draft.Media = []comment.Media{{Type: comment.MediaPhoto, FileID: "f1", FileUniqueID: "u1"}}
	draft.UserMessageID = 500
	draft.PromptMessageID = 501
	require.NoError(t, st.SaveDraft(ctx, draft))

	got, err = st.Draft(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, "привет", got.Body)
	assert.Equal(t, draft.Media, got.Media)
	assert.True(t, got.Staged())
	assert.Equal(t, 500, got.UserMessageID)
	assert.Equal(t, 501, got.PromptMessageID)
	assert.Zero(t, got.ReplyToCommentID, "a NULL parent reads back as zero, not as comment 0")

	require.NoError(t, st.DeleteDraft(ctx, userID))
	_, err = st.Draft(ctx, userID)
	require.ErrorIs(t, err, comment.ErrNotFound)

	require.NoError(t, st.DeleteDraft(ctx, userID), "deleting a missing draft is not an error")
}

func TestCommentPublishWritesIdentityMap(t *testing.T) {
	st, ctx := open(t)

	const (
		userID    = int64(10_003)
		messageID = 8801
	)

	_, err := st.EnsureUser(ctx, userID)
	require.NoError(t, err)

	id, err := st.CreateComment(ctx, comment.Comment{
		UserID:    userID,
		PostID:    4242,
		Nickname:  fox,
		Text:      "привет",
		Media:     []comment.Media{{Type: comment.MediaPhoto, FileID: "f1", FileUniqueID: "u1"}},
		Status:    comment.StatusPending,
		CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	require.NotZero(t, id)

	require.NoError(t, st.MarkCommentPublished(ctx, id, messageID))

	var (
		author     int64
		nickname   string
		commentID  int64
		status     string
		storedText string
	)

	err = probe(ctx, t).QueryRow(ctx, `
SELECT i.user_id, i.nickname, i.comment_id, c.status, c.content_text
FROM identity_map i JOIN comments c ON c.id = i.comment_id
WHERE i.message_id_in_group = $1`, messageID).
		Scan(&author, &nickname, &commentID, &status, &storedText)
	require.NoError(t, err)

	assert.Equal(t, userID, author, "identity_map is what lets a moderator trace an author")
	assert.Equal(t, fox, nickname, "the label is stored verbatim, not a foreign key")
	assert.Equal(t, id, commentID)
	assert.Equal(t, string(comment.StatusPublished), status)
	assert.Equal(t, "привет", storedText)

	// Telegram never reuses a message id, but a retry of the same publish must not break.
	require.NoError(t, st.MarkCommentPublished(ctx, id, messageID))
}

func TestCommentWithoutMedia(t *testing.T) {
	st, ctx := open(t)

	const userID = int64(10_004)

	_, err := st.EnsureUser(ctx, userID)
	require.NoError(t, err)

	// A nil slice must land as an empty JSON array, not NULL.
	id, err := st.CreateComment(ctx, comment.Comment{
		UserID:    userID,
		PostID:    4242,
		Nickname:  fox,
		Text:      "без медиа",
		Status:    comment.StatusPending,
		CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	var media string
	require.NoError(t, probe(ctx, t).
		QueryRow(ctx, `SELECT media_json::text FROM comments WHERE id = $1`, id).Scan(&media))
	assert.Equal(t, "[]", media)

	require.NoError(t, st.MarkCommentFailed(ctx, id))

	var status string
	require.NoError(t, probe(ctx, t).
		QueryRow(ctx, `SELECT status FROM comments WHERE id = $1`, id).Scan(&status))
	assert.Equal(t, string(comment.StatusFailed), status)
}

func TestMarkPublishedMissingComment(t *testing.T) {
	st, ctx := open(t)

	err := st.MarkCommentPublished(ctx, 999_999, 1)
	require.ErrorIs(t, err, comment.ErrNotFound)
}

func TestSaveReport(t *testing.T) {
	st, ctx := open(t)

	require.NoError(t, st.SaveReport(ctx, "comment", 1, 10_005, ""))
	require.NoError(t, st.SaveReport(ctx, "comment", 1, 10_006, "спам"))

	var n int
	require.NoError(t, probe(ctx, t).
		QueryRow(ctx, `SELECT count(*) FROM reports WHERE target_type = 'comment' AND target_id = 1`).Scan(&n))
	assert.Equal(t, 2, n, "every complaint is kept, duplicates included")
}

func TestReplyChain(t *testing.T) {
	st, ctx := open(t)

	const (
		author  = int64(10_007)
		replier = int64(10_008)
	)

	for _, id := range []int64{author, replier} {
		_, err := st.EnsureUser(ctx, id)
		require.NoError(t, err)
	}

	parentID, err := st.CreateComment(ctx, comment.Comment{
		UserID: author, PostID: 4242, Nickname: fox,
		Text: "первый", Status: comment.StatusPending, CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	require.NoError(t, st.MarkCommentPublished(ctx, parentID, 8800))

	parent, err := st.Comment(ctx, parentID)
	require.NoError(t, err)
	assert.Equal(t, 8800, parent.MessageID, "a reply needs this to hang off the right message")
	assert.Equal(t, comment.StatusPublished, parent.Status)
	assert.Zero(t, parent.ReplyToCommentID)

	replyID, err := st.CreateComment(ctx, comment.Comment{
		UserID: replier, PostID: 4242, Nickname: owl, ReplyToCommentID: parentID,
		Text: "второй", Status: comment.StatusPending, CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	reply, err := st.Comment(ctx, replyID)
	require.NoError(t, err)
	assert.Equal(t, parentID, reply.ReplyToCommentID, "the foreign key holds the chain together")

	_, err = st.Comment(ctx, 999_999)
	require.ErrorIs(t, err, comment.ErrNotFound)
}

func TestPrivateChatLog(t *testing.T) {
	st, ctx := open(t)

	const user = int64(10_009)

	empty, err := st.Messages(ctx, user)
	require.NoError(t, err)
	assert.Empty(t, empty)

	require.NoError(t, st.Remember(ctx, user, 7, 5, 6))

	got, err := st.Messages(ctx, user)
	require.NoError(t, err)
	assert.Equal(t, []int{5, 6, 7}, got, "oldest first, so a chat disappears from the top")

	// Every message in the chat is recorded, so a repeat must not fail.
	require.NoError(t, st.Remember(ctx, user, 5, 8))

	got, err = st.Messages(ctx, user)
	require.NoError(t, err)
	assert.Equal(t, []int{5, 6, 7, 8}, got)

	require.NoError(t, st.Forget(ctx, user, 5, 7))

	got, err = st.Messages(ctx, user)
	require.NoError(t, err)
	assert.Equal(t, []int{6, 8}, got)

	require.NoError(t, st.Remember(ctx, user), "an empty batch is a no-op")
	require.NoError(t, st.Forget(ctx, user), "and so is forgetting nothing")

	// The log is keyed by user and must not leak between chats.
	other, err := st.Messages(ctx, user+1)
	require.NoError(t, err)
	assert.Empty(t, other)
}

func TestNicknamesGatedByAchievements(t *testing.T) {
	st, ctx := open(t)

	const (
		plain   = int64(10_010)
		awarded = int64(10_011)
	)

	for _, id := range []int64{plain, awarded} {
		_, err := st.EnsureUser(ctx, id)
		require.NoError(t, err)
	}

	seeded, err := st.Nicknames(ctx, plain)
	require.NoError(t, err)
	require.NotEmpty(t, seeded, "the migration seeds the public masks")

	conn := probe(ctx, t)

	var lockedID int64
	require.NoError(t, conn.QueryRow(ctx,
		`INSERT INTO nicknames (code, label) VALUES ('mammoth', 'Мамонт') RETURNING id`).Scan(&lockedID))

	var achievementID int64
	require.NoError(t, conn.QueryRow(ctx,
		`INSERT INTO achievements (code, title) VALUES ('digger', 'Раскопки') RETURNING id`).Scan(&achievementID))

	_, err = conn.Exec(ctx,
		`INSERT INTO achievement_nicknames (achievement_id, nickname_id) VALUES ($1, $2)`,
		achievementID, lockedID)
	require.NoError(t, err)

	// Gated the moment an achievement points at it, for everyone.
	gated, err := st.Nicknames(ctx, plain)
	require.NoError(t, err)
	assert.Equal(t, seeded, gated, "a mask behind an achievement is hidden until it is earned")

	_, err = conn.Exec(ctx,
		`INSERT INTO user_achievements (user_id, achievement_id) VALUES ($1, $2)`,
		awarded, achievementID)
	require.NoError(t, err)

	unlocked, err := st.Nicknames(ctx, awarded)
	require.NoError(t, err)
	assert.Len(t, unlocked, len(seeded)+1)
	assert.Contains(t, labels(unlocked), "Мамонт")

	stillGated, err := st.Nicknames(ctx, plain)
	require.NoError(t, err)
	assert.NotContains(t, labels(stillGated), "Мамонт", "one person earning it does not open it for others")

	// Retiring a mask hides it without touching comments already published under it.
	_, err = conn.Exec(ctx, `UPDATE nicknames SET is_active = FALSE WHERE label = $1`, fox)
	require.NoError(t, err)

	active, err := st.Nicknames(ctx, plain)
	require.NoError(t, err)
	assert.NotContains(t, labels(active), fox)
}

func TestNicknameLabelMustFitACallback(t *testing.T) {
	st, ctx := open(t)
	_ = st

	// "nick:" takes five of Telegram's 64 callback_data bytes, so 59 is the limit.
	_, err := probe(ctx, t).Exec(ctx,
		`INSERT INTO nicknames (code, label) VALUES ('toolong', $1)`, strings.Repeat("я", 30))
	require.Error(t, err, "the schema refuses a label that could not be put on a button")
	assert.Contains(t, err.Error(), "nicknames_label_check")
}

func TestAchievementsAndActivity(t *testing.T) {
	st, ctx := open(t)

	const user = int64(10_012)

	_, err := st.EnsureUser(ctx, user)
	require.NoError(t, err)

	empty, err := st.Achievements(ctx, user)
	require.NoError(t, err)
	assert.Empty(t, empty)

	activity, err := st.Activity(ctx, user)
	require.NoError(t, err)
	assert.Equal(t, profile.Activity{}, activity)

	conn := probe(ctx, t)

	var achievementID int64
	require.NoError(t, conn.QueryRow(ctx,
		`INSERT INTO achievements (code, title, description) VALUES ('first', 'Первый', 'За первый комментарий')
		 RETURNING id`).Scan(&achievementID))
	_, err = conn.Exec(ctx,
		`INSERT INTO user_achievements (user_id, achievement_id) VALUES ($1, $2)`, user, achievementID)
	require.NoError(t, err)

	got, err := st.Achievements(ctx, user)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, profile.Achievement{Code: "first", Title: "Первый", Description: "За первый комментарий"}, got[0])

	// Two published comments and one published reply; a failed one is not counted.
	top := mustPublish(ctx, t, st, user, 0, 8801)
	mustPublish(ctx, t, st, user, 0, 8802)
	mustPublish(ctx, t, st, user, top, 8803)

	failed, err := st.CreateComment(ctx, comment.Comment{
		UserID: user, PostID: 4242, Nickname: fox,
		Text: "не ушло", Status: comment.StatusPending, CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	require.NoError(t, st.MarkCommentFailed(ctx, failed))

	activity, err = st.Activity(ctx, user)
	require.NoError(t, err)
	assert.Equal(t, profile.Activity{Comments: 2, Replies: 1}, activity)
}

// mustPublish creates a published comment and returns its id.
func mustPublish(ctx context.Context, t *testing.T, st *storage.Storage, user, parent int64, messageID int) int64 {
	t.Helper()

	id, err := st.CreateComment(ctx, comment.Comment{
		UserID: user, PostID: 4242, Nickname: fox, ReplyToCommentID: parent,
		Text: "текст", Status: comment.StatusPending, CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	require.NoError(t, st.MarkCommentPublished(ctx, id, messageID))

	return id
}

func labels(nicknames []comment.Nickname) []string {
	out := make([]string, 0, len(nicknames))
	for _, n := range nicknames {
		out = append(out, n.Label)
	}

	return out
}

func TestActiveRulesAndGrant(t *testing.T) {
	st, ctx := open(t)

	const user = int64(10_013)

	_, err := st.EnsureUser(ctx, user)
	require.NoError(t, err)

	none, err := st.ActiveRules(ctx)
	require.NoError(t, err)
	assert.Empty(t, none)

	conn := probe(ctx, t)

	var achievementID int64
	require.NoError(t, conn.QueryRow(ctx,
		`INSERT INTO achievements (code, title) VALUES ('night', 'Полуночник') RETURNING id`).Scan(&achievementID))

	_, err = conn.Exec(ctx, `
INSERT INTO achievement_rules (achievement_id, after_hour, before_hour, min_length, upper_only, pattern, min_comments)
VALUES ($1, 23, 5, 10, TRUE, '(?i)котик', 3)`, achievementID)
	require.NoError(t, err)

	// An inactive rule is invisible to the awarder.
	_, err = conn.Exec(ctx,
		`INSERT INTO achievement_rules (achievement_id, min_length, is_active) VALUES ($1, 1, FALSE)`,
		achievementID)
	require.NoError(t, err)

	rules, err := st.ActiveRules(ctx)
	require.NoError(t, err)
	require.Len(t, rules, 1)

	got := rules[0]
	assert.Equal(t, achievementID, got.AchievementID)
	assert.Equal(t, "night", got.AchievementCode)
	assert.Equal(t, "Полуночник", got.AchievementTitle)
	require.NotNil(t, got.AfterHour)
	assert.Equal(t, 23, *got.AfterHour)
	require.NotNil(t, got.BeforeHour)
	assert.Equal(t, 5, *got.BeforeHour)
	require.NotNil(t, got.MinLength)
	assert.Equal(t, 10, *got.MinLength)
	require.NotNil(t, got.UpperOnly)
	assert.True(t, *got.UpperOnly)
	assert.Equal(t, "(?i)котик", got.Pattern)
	require.NotNil(t, got.MinComments)
	assert.Equal(t, 3, *got.MinComments)

	// Columns left out stay nil, which is how "not checked" is expressed.
	assert.Nil(t, got.MaxLength)
	assert.Nil(t, got.MinReplies)
	assert.Nil(t, got.MinPosts)
	assert.Nil(t, got.MinAchievements)

	fresh, err := st.Grant(ctx, user, achievementID)
	require.NoError(t, err)
	assert.True(t, fresh)

	again, err := st.Grant(ctx, user, achievementID)
	require.NoError(t, err)
	assert.False(t, again, "a rule that keeps matching must not hand out the same reward twice")
}

func TestCountersFeedTheRules(t *testing.T) {
	st, ctx := open(t)

	const user = int64(10_014)

	_, err := st.EnsureUser(ctx, user)
	require.NoError(t, err)

	empty, err := st.Counters(ctx, user)
	require.NoError(t, err)
	assert.Equal(t, achievement.Counters{}, empty)

	top := mustPublish(ctx, t, st, user, 0, 8810)
	mustPublish(ctx, t, st, user, top, 8811)

	conn := probe(ctx, t)

	var achievementID int64
	require.NoError(t, conn.QueryRow(ctx,
		`INSERT INTO achievements (code, title) VALUES ('first', 'Первый') RETURNING id`).Scan(&achievementID))
	_, err = conn.Exec(ctx,
		`INSERT INTO user_achievements (user_id, achievement_id) VALUES ($1, $2)`, user, achievementID)
	require.NoError(t, err)

	// Published suggestions count as posts; one merely queued does not.
	_, err = conn.Exec(ctx,
		`INSERT INTO suggested_posts (user_id, content_text, status) VALUES ($1, 'текст', 'published')`, user)
	require.NoError(t, err)
	_, err = conn.Exec(ctx,
		`INSERT INTO suggested_posts (user_id, content_text, status) VALUES ($1, 'текст', 'approved')`, user)
	require.NoError(t, err)
	_, err = conn.Exec(ctx,
		`INSERT INTO suggested_posts (user_id, content_text, status) VALUES ($1, 'текст', 'pending')`, user)
	require.NoError(t, err)

	got, err := st.Counters(ctx, user)
	require.NoError(t, err)
	assert.Equal(t, achievement.Counters{Comments: 1, Replies: 1, Posts: 1, Achievements: 1}, got)
}

func TestRuleWithNoConditionIsRefused(t *testing.T) {
	st, ctx := open(t)
	_ = st

	conn := probe(ctx, t)

	var achievementID int64
	require.NoError(t, conn.QueryRow(ctx,
		`INSERT INTO achievements (code, title) VALUES ('empty', 'Пустое') RETURNING id`).Scan(&achievementID))

	// Such a rule would fire on every comment ever published.
	_, err := conn.Exec(ctx, `INSERT INTO achievement_rules (achievement_id) VALUES ($1)`, achievementID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "achievement_rules_not_empty")

	// Half an hour window is meaningless too.
	_, err = conn.Exec(ctx,
		`INSERT INTO achievement_rules (achievement_id, after_hour) VALUES ($1, 23)`, achievementID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "achievement_rules_hours_paired")
}

func TestSuggestionLifecycle(t *testing.T) {
	st, ctx := open(t)

	const user = int64(10_015)

	_, err := st.EnsureUser(ctx, user)
	require.NoError(t, err)

	_, err = st.Suggestion(ctx, 999_999)
	require.ErrorIs(t, err, suggestion.ErrNotFound)

	created := time.Now().UTC().Truncate(time.Millisecond)
	id, err := st.CreateSuggestion(ctx, suggestion.Suggestion{
		UserID:    user,
		Text:      "предлагаю пост",
		Media:     []comment.Media{{Type: comment.MediaPhoto, FileID: "f1", FileUniqueID: "u1"}},
		Status:    suggestion.StatusPending,
		CreatedAt: created,
	})
	require.NoError(t, err)
	require.NotZero(t, id)

	got, err := st.Suggestion(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, user, got.UserID)
	assert.Equal(t, "предлагаю пост", got.Text)
	assert.Equal(t, suggestion.StatusPending, got.Status)
	require.Len(t, got.Media, 1)
	assert.Equal(t, "f1", got.Media[0].FileID)
	assert.Zero(t, got.ModerationMessageID, "a NULL card id reads back as zero")
	assert.Nil(t, got.EditedAt, "an untouched post has no edit time")
	assert.WithinDuration(t, created, got.CreatedAt, time.Millisecond)

	require.NoError(t, st.SetModerationMessage(ctx, id, 500))

	// An edit keeps the row and its card, and records when it happened.
	editedAt := time.Now().UTC().Truncate(time.Millisecond)
	require.NoError(t, st.UpdateSuggestionText(ctx, id, "другой текст", nil, editedAt))

	got, err = st.Suggestion(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "другой текст", got.Text)
	assert.Empty(t, got.Media, "a nil slice reads back empty, not null")
	assert.Equal(t, 500, got.ModerationMessageID)
	require.NotNil(t, got.EditedAt)

	waiting, err := st.SuggestionsByStatus(ctx, user, suggestion.StatusPending)
	require.NoError(t, err)
	require.Len(t, waiting, 1)

	// A pending suggestion is not an approved post.
	counters, err := st.Counters(ctx, user)
	require.NoError(t, err)
	assert.Zero(t, counters.Posts)

	// Approved but not yet sent is not a post.
	require.NoError(t, st.SetSuggestionStatus(ctx, id, suggestion.StatusApproved, time.Now().UTC()))

	counters, err = st.Counters(ctx, user)
	require.NoError(t, err)
	assert.Zero(t, counters.Posts, "an approval that never reached the channel is not a post")

	require.NoError(t, st.SetChannelMessage(ctx, id, 4242))
	require.NoError(t, st.SetSuggestionStatus(ctx, id, suggestion.StatusPublished, time.Now().UTC()))

	got, err = st.Suggestion(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, suggestion.StatusPublished, got.Status)
	assert.Equal(t, 4242, got.ChannelMessageID)

	published, err := st.SuggestionsByStatus(ctx, user, suggestion.StatusPublished)
	require.NoError(t, err)
	require.Len(t, published, 1)

	waiting, err = st.SuggestionsByStatus(ctx, user, suggestion.StatusPending)
	require.NoError(t, err)
	assert.Empty(t, waiting)

	counters, err = st.Counters(ctx, user)
	require.NoError(t, err)
	assert.Equal(t, 1, counters.Posts, "a published suggestion is what a post rule counts")

	assert.Equal(t, 1, mustCount(ctx, t, st, user, suggestion.StatusPublished))
	assert.Zero(t, mustCount(ctx, t, st, user, suggestion.StatusApproved))
}

func TestSuggestionStatusIsConstrained(t *testing.T) {
	st, ctx := open(t)

	const user = int64(10_016)

	_, err := st.EnsureUser(ctx, user)
	require.NoError(t, err)

	id, err := st.CreateSuggestion(ctx, suggestion.Suggestion{
		UserID: user, Text: "текст", Status: suggestion.StatusPending, CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, err = probe(ctx, t).Exec(ctx, `UPDATE suggested_posts SET status = 'whatever' WHERE id = $1`, id)
	require.Error(t, err, "the schema refuses a status the bot cannot produce")
	assert.Contains(t, err.Error(), "suggested_posts_status_check")
}

func TestSuggestionDrafts(t *testing.T) {
	st, ctx := open(t)

	const user = int64(10_017)

	_, err := st.EnsureUser(ctx, user)
	require.NoError(t, err)

	_, err = st.SuggestionDraft(ctx, user)
	require.ErrorIs(t, err, suggestion.ErrNotFound)

	require.NoError(t, st.SaveSuggestionDraft(ctx, suggestion.Draft{UserID: user, CreatedAt: time.Now().UTC()}))

	draft, err := st.SuggestionDraft(ctx, user)
	require.NoError(t, err)
	assert.False(t, draft.Editing(), "a NULL editing_id reads back as zero, not as post 0")

	id, err := st.CreateSuggestion(ctx, suggestion.Suggestion{
		UserID: user, Text: "текст", Status: suggestion.StatusPending, CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	require.NoError(t, st.SaveSuggestionDraft(ctx, suggestion.Draft{
		UserID: user, EditingID: id, CreatedAt: time.Now().UTC(),
	}))

	draft, err = st.SuggestionDraft(ctx, user)
	require.NoError(t, err)
	assert.True(t, draft.Editing())
	assert.Equal(t, id, draft.EditingID)

	require.NoError(t, st.DeleteSuggestionDraft(ctx, user))
	_, err = st.SuggestionDraft(ctx, user)
	require.ErrorIs(t, err, suggestion.ErrNotFound)
}

func TestRuleEventColumn(t *testing.T) {
	st, ctx := open(t)

	conn := probe(ctx, t)

	var achievementID int64
	require.NoError(t, conn.QueryRow(ctx,
		`INSERT INTO achievements (code, title) VALUES ('author', 'Автор') RETURNING id`).Scan(&achievementID))

	// A rule written before suggested posts existed gets the comment default.
	_, err := conn.Exec(ctx,
		`INSERT INTO achievement_rules (achievement_id, min_length) VALUES ($1, 10)`, achievementID)
	require.NoError(t, err)

	_, err = conn.Exec(ctx,
		`INSERT INTO achievement_rules (achievement_id, event, min_posts) VALUES ($1, 'post', 5)`, achievementID)
	require.NoError(t, err)

	rules, err := st.ActiveRules(ctx)
	require.NoError(t, err)
	require.Len(t, rules, 2)
	assert.Equal(t, achievement.KindComment, rules[0].Event, "the default keeps old rules about comments")
	assert.Equal(t, achievement.KindPost, rules[1].Event)

	_, err = conn.Exec(ctx,
		`INSERT INTO achievement_rules (achievement_id, event, min_length) VALUES ($1, 'telepathy', 1)`,
		achievementID)
	require.Error(t, err, "the schema refuses a kind the bot cannot produce")
}

// mustCount is CountSuggestions without the error handling at every call site.
func mustCount(
	ctx context.Context, t *testing.T, st *storage.Storage, userID int64, statuses ...suggestion.Status,
) int {
	t.Helper()

	n, err := st.CountSuggestions(ctx, userID, statuses...)
	require.NoError(t, err)

	return n
}

func TestActivityCountsPostsByStage(t *testing.T) {
	st, ctx := open(t)

	const user = int64(10_018)

	_, err := st.EnsureUser(ctx, user)
	require.NoError(t, err)

	conn := probe(ctx, t)
	for _, status := range []string{"published", "published", "pending", "declined"} {
		_, err := conn.Exec(ctx,
			`INSERT INTO suggested_posts (user_id, content_text, status) VALUES ($1, 'текст', $2)`,
			user, status)
		require.NoError(t, err)
	}

	got, err := st.Activity(ctx, user)
	require.NoError(t, err)

	assert.Equal(t, 2, got.Posts)
	assert.Equal(t, 1, got.PostsPending)
	assert.Zero(t, got.Comments, "a declined post is counted nowhere")
}
