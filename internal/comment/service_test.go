package comment_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"loudbot/internal/comment"
)

const (
	userID       = int64(777)
	postID       = 42
	discussionID = int64(-100500)
	threadMsgID  = 9001
	channelID    = int64(-1001234567890)
)

var (
	fox = comment.Nickname{Label: "Лис"}
	owl = comment.Nickname{Label: "Сова"}

	now = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
)

func testOptions() comment.Options {
	return comment.Options{
		BotUsername:   "anon_bot",
		ReplyLinkText: "ответить",
		ChannelID:     channelID,
		MaxTextLen:    10,
		DraftTTL:      time.Hour,
	}
}

func newService(t *testing.T, repo *fakeRepo, pub *fakePublisher, guard comment.Guard) *comment.Service {
	t.Helper()

	svc := comment.New(repo, pub, guard, testOptions())
	svc.SetClock(func() time.Time { return now })

	return svc
}

func testPost() comment.Post {
	return comment.Post{
		ChannelMessageID:    postID,
		DiscussionChatID:    discussionID,
		DiscussionMessageID: threadMsgID,
		Body:                "Текст поста",
		ChannelUsername:     "anon_channel",
	}
}

// fullRepo is the happy-path world: an active user and a post with a thread.
func fullRepo() *fakeRepo {
	return newRepo().withUser(comment.User{ID: userID}).withPost(testPost()).withNicknames(fox, owl)
}

// openDraft is a draft bound to the post with nothing written yet.
func openDraft() comment.Draft {
	return comment.Draft{UserID: userID, PostID: postID, CreatedAt: now}
}

// stagedDraft is a draft whose message is waiting for a mask.
func stagedDraft() comment.Draft {
	d := openDraft()
	d.Body = "привет"
	d.UserMessageID = 500
	d.PromptMessageID = 501

	return d
}

func TestStartBindsAuthorToPost(t *testing.T) {
	t.Parallel()

	repo := fullRepo()
	svc := newService(t, repo, &fakePublisher{}, nil)

	got, err := svc.Start(context.Background(), userID, "comment_42")
	require.NoError(t, err)

	assert.Equal(t, "Текст поста", got.Post.Body, "the transport quotes the post back to the author")
	assert.Equal(t, "https://t.me/anon_channel/42", got.Link)

	assert.Equal(t, openDraft(), repo.drafts[userID], "no mask is chosen yet, and nothing is staged")
}

func TestStartErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		repo    func() *fakeRepo
		opts    func(*comment.Options)
		payload string
		wantErr error
	}{
		{name: "malformed payload", repo: fullRepo, payload: "hello", wantErr: comment.ErrBadPayload},
		{
			name:    "banned user",
			repo:    func() *fakeRepo { return fullRepo().withUser(comment.User{ID: userID, Banned: true}) },
			payload: "comment_42",
			wantErr: comment.ErrBanned,
		},
		{name: "unknown post", repo: fullRepo, payload: "comment_43", wantErr: comment.ErrUnknownPost},
		{
			name: "post without discussion thread",
			repo: func() *fakeRepo {
				return fullRepo().withPost(comment.Post{ChannelMessageID: postID, DiscussionChatID: discussionID})
			},
			payload: "comment_42",
			wantErr: comment.ErrUnknownPost,
		},
		{
			name:    "user has unlocked no masks",
			repo:    func() *fakeRepo { return fullRepo().withNicknames() },
			payload: "comment_42",
			wantErr: comment.ErrNoNicknames,
		},
		{
			name:    "repository failure",
			repo:    func() *fakeRepo { r := fullRepo(); r.ensureUserErr = errBoom; return r },
			payload: "comment_42",
			wantErr: errBoom,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := tc.repo()
			opts := testOptions()
			if tc.opts != nil {
				tc.opts(&opts)
			}

			svc := comment.New(repo, &fakePublisher{}, nil, opts)
			svc.SetClock(func() time.Time { return now })

			_, err := svc.Start(context.Background(), userID, tc.payload)
			require.ErrorIs(t, err, tc.wantErr)
			assert.Empty(t, repo.drafts, "a failed start must not leave a draft behind")
		})
	}
}

func TestStageStoresMessageAndOffersMasks(t *testing.T) {
	t.Parallel()

	repo := fullRepo().withDraft(openDraft())
	guard := &recordingGuard{}
	svc := newService(t, repo, &fakePublisher{}, guard)

	media := []comment.Media{{Type: comment.MediaPhoto, FileID: "f1"}}

	got, err := svc.Stage(context.Background(), comment.StageRequest{
		UserID:    userID,
		MessageID: 500,
		Text:      "  привет  ",
		Media:     media,
	})
	require.NoError(t, err)

	assert.Equal(t, []comment.Nickname{fox, owl}, got.Nicknames)
	assert.Zero(t, got.StaleMessageID, "nothing was staged before")
	assert.Zero(t, got.StalePromptID)

	stored := repo.drafts[userID]
	assert.Equal(t, "привет", stored.Body, "the stored text is trimmed")
	assert.Equal(t, media, stored.Media)
	assert.Equal(t, 500, stored.UserMessageID)
	assert.True(t, stored.Staged())

	require.Len(t, guard.requests, 1)
	assert.Equal(t, comment.GuardRequest{UserID: userID, PostID: postID, Text: "привет", HasMedia: true}, guard.requests[0])
}

func TestStagePutsLastMaskFirst(t *testing.T) {
	t.Parallel()

	repo := fullRepo().
		withUser(comment.User{ID: userID, LastNickname: owl.Label}).
		withDraft(openDraft())
	svc := newService(t, repo, &fakePublisher{}, nil)

	got, err := svc.Stage(context.Background(), comment.StageRequest{UserID: userID, MessageID: 500, Text: "hi"})
	require.NoError(t, err)
	assert.Equal(t, []comment.Nickname{owl, fox}, got.Nicknames)
}

func TestStageKeepsConfigOrderWhenLastMaskIsGone(t *testing.T) {
	t.Parallel()

	repo := fullRepo().
		withUser(comment.User{ID: userID, LastNickname: "Мамонт"}).
		withDraft(openDraft())
	svc := newService(t, repo, &fakePublisher{}, nil)

	got, err := svc.Stage(context.Background(), comment.StageRequest{UserID: userID, MessageID: 500, Text: "hi"})
	require.NoError(t, err)
	assert.Equal(t, []comment.Nickname{fox, owl}, got.Nicknames)
}

func TestStageReplacesAnEarlierMessage(t *testing.T) {
	t.Parallel()

	repo := fullRepo().withDraft(stagedDraft())
	svc := newService(t, repo, &fakePublisher{}, nil)

	got, err := svc.Stage(context.Background(), comment.StageRequest{UserID: userID, MessageID: 600, Text: "ещё раз"})
	require.NoError(t, err)

	assert.Equal(t, 500, got.StaleMessageID, "the transport takes the superseded pair off screen")
	assert.Equal(t, 501, got.StalePromptID)

	stored := repo.drafts[userID]
	assert.Equal(t, "ещё раз", stored.Body, "writing again replaces the draft rather than queueing a second comment")
	assert.Equal(t, 600, stored.UserMessageID)
	assert.Zero(t, stored.PromptMessageID, "the new keyboard is attached separately once it is sent")
}

func TestStageErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		repo    func() *fakeRepo
		req     comment.StageRequest
		wantErr error
	}{
		{
			name:    "no draft",
			repo:    fullRepo,
			req:     comment.StageRequest{UserID: userID, MessageID: 1, Text: "hi"},
			wantErr: comment.ErrNoDraft,
		},
		{
			name: "expired draft",
			repo: func() *fakeRepo {
				d := openDraft()
				d.CreatedAt = now.Add(-2 * time.Hour)
				return fullRepo().withDraft(d)
			},
			req:     comment.StageRequest{UserID: userID, MessageID: 1, Text: "hi"},
			wantErr: comment.ErrDraftExpired,
		},
		{
			name: "banned user",
			repo: func() *fakeRepo {
				return fullRepo().withUser(comment.User{ID: userID, Banned: true}).withDraft(openDraft())
			},
			req:     comment.StageRequest{UserID: userID, MessageID: 1, Text: "hi"},
			wantErr: comment.ErrBanned,
		},
		{
			name:    "empty message",
			repo:    func() *fakeRepo { return fullRepo().withDraft(openDraft()) },
			req:     comment.StageRequest{UserID: userID, MessageID: 1, Text: "   "},
			wantErr: comment.ErrEmptyComment,
		},
		{
			name:    "text too long",
			repo:    func() *fakeRepo { return fullRepo().withDraft(openDraft()) },
			req:     comment.StageRequest{UserID: userID, MessageID: 1, Text: strings.Repeat("a", 11)},
			wantErr: comment.ErrTextTooLong,
		},
		{
			name: "post deleted while composing",
			repo: func() *fakeRepo {
				return newRepo().withUser(comment.User{ID: userID}).withNicknames(fox, owl).withDraft(openDraft())
			},
			req:     comment.StageRequest{UserID: userID, MessageID: 1, Text: "hi"},
			wantErr: comment.ErrUnknownPost,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := tc.repo()
			svc := newService(t, repo, &fakePublisher{}, nil)

			_, err := svc.Stage(context.Background(), tc.req)
			require.ErrorIs(t, err, tc.wantErr)

			if d, ok := repo.drafts[userID]; ok {
				assert.False(t, d.Staged(), "a refused message must not be left staged")
			}
		})
	}
}

func TestStageCountsRunesNotBytes(t *testing.T) {
	t.Parallel()

	repo := fullRepo().withDraft(openDraft())
	svc := newService(t, repo, &fakePublisher{}, nil)
	ctx := context.Background()

	// MaxTextLen is 10: ten Cyrillic runes are 20 bytes and must still pass.
	_, err := svc.Stage(ctx, comment.StageRequest{UserID: userID, MessageID: 1, Text: strings.Repeat("я", 10)})
	require.NoError(t, err)

	_, err = svc.Stage(ctx, comment.StageRequest{UserID: userID, MessageID: 2, Text: strings.Repeat("я", 11)})
	require.ErrorIs(t, err, comment.ErrTextTooLong)
}

func TestStageRejectedByGuard(t *testing.T) {
	t.Parallel()

	repo := fullRepo().withDraft(openDraft())
	guard := &blockingGuard{reason: "слишком часто"}
	svc := newService(t, repo, &fakePublisher{}, guard)

	_, err := svc.Stage(context.Background(), comment.StageRequest{UserID: userID, MessageID: 1, Text: "hi"})

	rejected, ok := comment.Rejected(err)
	require.True(t, ok, "the caller must be able to show the reason to the author")
	assert.Equal(t, "слишком часто", rejected.Reason)
	assert.False(t, repo.drafts[userID].Staged(), "a rejected message is never staged")
}

func TestAttachPrompt(t *testing.T) {
	t.Parallel()

	draft := stagedDraft()
	draft.PromptMessageID = 0
	repo := fullRepo().withDraft(draft)
	svc := newService(t, repo, &fakePublisher{}, nil)

	require.NoError(t, svc.AttachPrompt(context.Background(), userID, 900))
	assert.Equal(t, 900, repo.drafts[userID].PromptMessageID)
	assert.Equal(t, "привет", repo.drafts[userID].Body, "attaching the keyboard leaves the message alone")
}

func TestPublish(t *testing.T) {
	t.Parallel()

	draft := stagedDraft()
	draft.Media = []comment.Media{{Type: comment.MediaPhoto, FileID: "f1"}}
	repo := fullRepo().withDraft(draft)
	pub := &fakePublisher{result: comment.PublishResult{MessageID: 555}}
	svc := newService(t, repo, pub, nil)

	got, err := svc.Publish(context.Background(), userID, owl.Label)
	require.NoError(t, err)

	assert.Equal(t, comment.StatusPublished, got.Status)
	assert.Equal(t, 555, got.MessageID)
	assert.Equal(t, owl.Label, got.Nickname)
	assert.Equal(t, "привет", got.Text, "the stored text carries no markup")

	require.Len(t, pub.requests, 1)
	assert.Equal(t, comment.PublishRequest{
		ChatID:           discussionID,
		ReplyToMessageID: threadMsgID,
		Text:             "<b>Сова</b>\n\nпривет\n\n<a href=\"https://t.me/anon_bot?start=reply_1\">ответить</a>",
		Media:            draft.Media,
	}, pub.requests[0], "the comment lands in the post's thread, signed in bold, with its own reply link")

	assert.Equal(t, owl.Label, repo.users[userID].LastNickname, "the mask is remembered for next time")

	left := repo.drafts[userID]
	assert.False(t, left.Staged(), "publishing clears the staged message")
	assert.Equal(t, postID, left.PostID, "the post binding survives, so the author can write again")
	assert.Empty(t, left.Body)
}

func TestPublishErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		repo    func() *fakeRepo
		label   string
		wantErr error
	}{
		{name: "no draft", repo: fullRepo, label: fox.Label, wantErr: comment.ErrNoDraft},
		{
			name:    "nothing staged yet",
			repo:    func() *fakeRepo { return fullRepo().withDraft(openDraft()) },
			label:   fox.Label,
			wantErr: comment.ErrNothingStaged,
		},
		{
			name:    "mask not available to this user",
			repo:    func() *fakeRepo { return fullRepo().withDraft(stagedDraft()) },
			label:   "Мамонт",
			wantErr: comment.ErrNicknameUnavailable,
		},
		{
			name: "banned while composing",
			repo: func() *fakeRepo {
				return fullRepo().withUser(comment.User{ID: userID, Banned: true}).withDraft(stagedDraft())
			},
			label:   fox.Label,
			wantErr: comment.ErrBanned,
		},
		{
			name: "post deleted while composing",
			repo: func() *fakeRepo {
				return newRepo().withUser(comment.User{ID: userID}).withNicknames(fox, owl).withDraft(stagedDraft())
			},
			label:   fox.Label,
			wantErr: comment.ErrUnknownPost,
		},
		{
			name:    "create comment fails",
			repo:    func() *fakeRepo { r := fullRepo().withDraft(stagedDraft()); r.createErr = errBoom; return r },
			label:   fox.Label,
			wantErr: errBoom,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pub := &fakePublisher{result: comment.PublishResult{MessageID: 1}}
			svc := newService(t, tc.repo(), pub, nil)

			_, err := svc.Publish(context.Background(), userID, tc.label)
			require.ErrorIs(t, err, tc.wantErr)
			assert.Empty(t, pub.requests, "nothing is published when the call is refused")
		})
	}
}

func TestPublishMarksCommentFailedWhenTelegramRefuses(t *testing.T) {
	t.Parallel()

	repo := fullRepo().withDraft(stagedDraft())
	pub := &fakePublisher{err: errBoom}
	svc := newService(t, repo, pub, nil)

	_, err := svc.Publish(context.Background(), userID, fox.Label)
	require.ErrorIs(t, err, errBoom)

	require.Len(t, repo.comments, 1)
	for _, c := range repo.comments {
		assert.Equal(t, comment.StatusFailed, c.Status, "a pending row must not be left behind")
	}

	assert.True(t, repo.drafts[userID].Staged(), "the message survives a transport failure so the author can retry")
}

func TestCancelTakesTheStagedMessageOffScreen(t *testing.T) {
	t.Parallel()

	repo := fullRepo().withDraft(stagedDraft())
	svc := newService(t, repo, &fakePublisher{}, nil)

	got, err := svc.Cancel(context.Background(), userID)
	require.NoError(t, err)

	assert.Equal(t, userID, got.ChatID)
	assert.Equal(t, 500, got.UserMessageID)
	assert.Equal(t, 501, got.PromptMessageID)

	left := repo.drafts[userID]
	assert.False(t, left.Staged())
	assert.Empty(t, left.Body)
	assert.Equal(t, postID, left.PostID, "cancelling returns to writing, still under the same post")
}

func TestCancelErrors(t *testing.T) {
	t.Parallel()

	svc := newService(t, fullRepo(), &fakePublisher{}, nil)
	_, err := svc.Cancel(context.Background(), userID)
	require.ErrorIs(t, err, comment.ErrNoDraft)

	svc = newService(t, fullRepo().withDraft(openDraft()), &fakePublisher{}, nil)
	_, err = svc.Cancel(context.Background(), userID)
	require.ErrorIs(t, err, comment.ErrNothingStaged)
}

func TestExpiredDraftIsDropped(t *testing.T) {
	t.Parallel()

	stale := stagedDraft()
	stale.CreatedAt = now.Add(-2 * time.Hour)
	repo := fullRepo().withDraft(stale)
	svc := newService(t, repo, &fakePublisher{}, nil)

	_, err := svc.Publish(context.Background(), userID, fox.Label)
	require.ErrorIs(t, err, comment.ErrDraftExpired)
	assert.Empty(t, repo.drafts, "an expired draft is removed so the next tap starts clean")
}

func TestDraftAtExactTTLIsStillValid(t *testing.T) {
	t.Parallel()

	draft := stagedDraft()
	draft.CreatedAt = now.Add(-time.Hour)
	repo := fullRepo().withDraft(draft)
	svc := newService(t, repo, &fakePublisher{result: comment.PublishResult{MessageID: 1}}, nil)

	_, err := svc.Publish(context.Background(), userID, fox.Label)
	require.NoError(t, err)
}

func TestDefaultsAreApplied(t *testing.T) {
	t.Parallel()

	repo := fullRepo().withDraft(stagedDraft())
	pub := &fakePublisher{result: comment.PublishResult{MessageID: 1}}

	// The mask list is required input; everything else must fall back to a default
	// rather than rejecting every comment via a zero-length limit.
	svc := comment.New(repo, pub, nil, comment.Options{
		BotUsername:   "anon_bot",
		ReplyLinkText: "ответить",
	})
	svc.SetClock(func() time.Time { return now })

	_, err := svc.Publish(context.Background(), userID, fox.Label)
	require.NoError(t, err)
	assert.Equal(t, "<b>Лис</b>\n\nпривет\n\n<a href=\"https://t.me/anon_bot?start=reply_1\">ответить</a>", pub.requests[0].Text)
}

func TestNicknames(t *testing.T) {
	t.Parallel()

	svc := newService(t, fullRepo(), &fakePublisher{}, nil)

	ctx := context.Background()

	got, err := svc.Nicknames(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, []comment.Nickname{fox, owl}, got)

	found, err := svc.Nickname(ctx, userID, owl.Label)
	require.NoError(t, err)
	assert.Equal(t, owl, found)

	_, err = svc.Nickname(ctx, userID, "Мамонт")
	require.ErrorIs(t, err, comment.ErrNicknameUnavailable)
}

func TestNicknameRefusesAMaskTheUserHasNotUnlocked(t *testing.T) {
	t.Parallel()

	// The label travels back from the client in callback_data, so a locked mask
	// must be refused at publish time, not merely left off the keyboard.
	draft := stagedDraft()
	repo := fullRepo().withNicknames(fox).withDraft(draft)
	pub := &fakePublisher{result: comment.PublishResult{MessageID: 1}}
	svc := newService(t, repo, pub, nil)

	_, err := svc.Publish(context.Background(), userID, owl.Label)
	require.ErrorIs(t, err, comment.ErrNicknameUnavailable)
	assert.Empty(t, pub.requests, "a locked mask never reaches the group")
}

func TestServiceDeepLinkUsesConfiguredUsername(t *testing.T) {
	t.Parallel()

	svc := newService(t, fullRepo(), &fakePublisher{}, nil)
	assert.Equal(t, "https://t.me/anon_bot?start=comment_42", svc.DeepLink(postID))
}

func TestOnPostPublishedLinksAndInvites(t *testing.T) {
	t.Parallel()

	repo := newRepo().withNicknames(fox, owl)
	pub := &fakePublisher{inviteResult: comment.PublishResult{MessageID: 4321}}
	svc := newService(t, repo, pub, nil)

	require.NoError(t, svc.OnPostPublished(context.Background(), testPost()))

	require.Len(t, pub.invites, 1)
	assert.Equal(t, comment.InviteRequest{
		ChatID:           discussionID,
		ReplyToMessageID: threadMsgID,
		DeepLink:         "https://t.me/anon_bot?start=comment_42",
	}, pub.invites[0], "the invitation replies to the forwarded post so it lands in the thread")

	stored := repo.posts[postID]
	assert.Equal(t, 4321, stored.InviteMessageID, "the invite id is what makes a redelivery a no-op")
	assert.Equal(t, "Текст поста", stored.Body, "the post body is kept for quoting it to authors")
	assert.Equal(t, "anon_channel", stored.ChannelUsername)
}

func TestOnPostPublishedIsIdempotent(t *testing.T) {
	t.Parallel()

	repo := newRepo().withNicknames(fox, owl)
	pub := &fakePublisher{inviteResult: comment.PublishResult{MessageID: 4321}}
	svc := newService(t, repo, pub, nil)

	ctx := context.Background()
	require.NoError(t, svc.OnPostPublished(ctx, testPost()))
	require.NoError(t, svc.OnPostPublished(ctx, testPost()), "Telegram redelivers auto-forwards")

	assert.Len(t, pub.invites, 1, "readers must not see two invitations under one post")
}

func TestOnPostPublishedRetriesInviteAfterFailure(t *testing.T) {
	t.Parallel()

	repo := newRepo()
	pub := &fakePublisher{inviteErr: errBoom}
	svc := newService(t, repo, pub, nil)

	ctx := context.Background()
	require.ErrorIs(t, svc.OnPostPublished(ctx, testPost()), errBoom)

	// The post is linked even though the invitation failed, so comments still work
	// through the button under the post itself.
	assert.Equal(t, threadMsgID, repo.posts[postID].DiscussionMessageID)
	assert.Zero(t, repo.posts[postID].InviteMessageID)

	pub.inviteErr = nil
	pub.inviteResult = comment.PublishResult{MessageID: 4321}

	require.NoError(t, svc.OnPostPublished(ctx, testPost()))
	assert.Len(t, pub.invites, 2)
	assert.Equal(t, 4321, repo.posts[postID].InviteMessageID)
}

func TestOnPostPublishedRepositoryErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		repo func() *fakeRepo
	}{
		{name: "lookup fails", repo: func() *fakeRepo { r := newRepo(); r.postErr = errBoom; return r }},
		{name: "link fails", repo: func() *fakeRepo { r := newRepo(); r.linkPostErr = errBoom; return r }},
		{name: "mark invite fails", repo: func() *fakeRepo { r := newRepo(); r.markInviteErr = errBoom; return r }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pub := &fakePublisher{inviteResult: comment.PublishResult{MessageID: 1}}
			svc := newService(t, tc.repo(), pub, nil)

			require.ErrorIs(t, svc.OnPostPublished(context.Background(), testPost()), errBoom)
		})
	}
}

// publishedComment is a comment already live in the thread, the kind an author
// can answer through its "ответить" link.
func publishedComment() comment.Comment {
	return comment.Comment{
		ID:        7,
		UserID:    999,
		PostID:    postID,
		Nickname:  owl.Label,
		MessageID: 8800,
		Text:      "а где продолжение?",
		Status:    comment.StatusPublished,
	}
}

func TestStartReplyBindsToTheComment(t *testing.T) {
	t.Parallel()

	parent := publishedComment()
	repo := fullRepo().withComment(parent)
	svc := newService(t, repo, &fakePublisher{}, nil)

	got, err := svc.Start(context.Background(), userID, "reply_7")
	require.NoError(t, err)

	require.True(t, got.IsReply())
	assert.Equal(t, parent.Text, got.ReplyTo.Text, "the transport quotes the comment being answered")
	assert.Equal(t, owl.Label, got.ReplyTo.Nickname)
	assert.Equal(t, postID, got.Post.ChannelMessageID, "a reply belongs to the same post")

	stored := repo.drafts[userID]
	assert.Equal(t, int64(7), stored.ReplyToCommentID)
	assert.Equal(t, postID, stored.PostID)
}

func TestStartReplyErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		repo    func() *fakeRepo
		wantErr error
	}{
		{
			name:    "comment does not exist",
			repo:    fullRepo,
			wantErr: comment.ErrUnknownComment,
		},
		{
			name: "comment never reached the group",
			repo: func() *fakeRepo {
				c := publishedComment()
				c.Status = comment.StatusFailed
				c.MessageID = 0

				return fullRepo().withComment(c)
			},
			wantErr: comment.ErrUnknownComment,
		},
		{
			name: "post gone while the link was open",
			repo: func() *fakeRepo {
				return newRepo().
					withUser(comment.User{ID: userID}).
					withNicknames(fox, owl).
					withComment(publishedComment())
			},
			wantErr: comment.ErrUnknownPost,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := tc.repo()
			svc := newService(t, repo, &fakePublisher{}, nil)

			_, err := svc.Start(context.Background(), userID, "reply_7")
			require.ErrorIs(t, err, tc.wantErr)
			assert.Empty(t, repo.drafts, "a failed start must not leave a draft behind")
		})
	}
}

func TestPublishReplyHangsOffTheParentMessage(t *testing.T) {
	t.Parallel()

	parent := publishedComment()

	draft := stagedDraft()
	draft.ReplyToCommentID = parent.ID

	repo := fullRepo().withComment(parent).withDraft(draft)
	pub := &fakePublisher{result: comment.PublishResult{MessageID: 9100}}
	svc := newService(t, repo, pub, nil)

	got, err := svc.Publish(context.Background(), userID, fox.Label)
	require.NoError(t, err)

	assert.Equal(t, parent.ID, got.ReplyToCommentID)

	require.Len(t, pub.requests, 1)
	assert.Equal(t, parent.MessageID, pub.requests[0].ReplyToMessageID,
		"a reply hangs off the comment it answers, not off the post")
	assert.Contains(t, pub.requests[0].Text, "start=reply_"+strconv.FormatInt(got.ID, 10),
		"the reply carries its own link, so it can be answered in turn")
}

func TestPublishReplyToDeletedComment(t *testing.T) {
	t.Parallel()

	draft := stagedDraft()
	draft.ReplyToCommentID = 7

	repo := fullRepo().withDraft(draft)
	pub := &fakePublisher{result: comment.PublishResult{MessageID: 1}}
	svc := newService(t, repo, pub, nil)

	_, err := svc.Publish(context.Background(), userID, fox.Label)
	require.ErrorIs(t, err, comment.ErrUnknownComment)
	assert.Empty(t, pub.requests, "nothing is published when the parent is gone")
	assert.Empty(t, repo.comments, "and no row is left behind for it")
}

func TestReplyNotice(t *testing.T) {
	t.Parallel()

	parent := publishedComment()
	reply := comment.Comment{
		ID: 8, UserID: userID, PostID: postID, Nickname: fox.Label,
		ReplyToCommentID: parent.ID, MessageID: 9100, Text: "ответ",
		Status: comment.StatusPublished,
	}

	repo := fullRepo().withComment(parent)
	svc := newService(t, repo, &fakePublisher{}, nil)

	got, ok, err := svc.ReplyNotice(context.Background(), reply)
	require.NoError(t, err)
	require.True(t, ok)

	assert.Equal(t, parent.UserID, got.RecipientID, "the parent's author is told, not the replier")
	assert.Equal(t, fox.Label, got.Nickname, "only the mask is revealed")
	assert.Equal(t, "ответ", got.Text)
	assert.Equal(t, "https://t.me/anon_channel/42?comment=9100", got.Link, "the link opens the reply itself")
}

func TestReplyNoticeHasNobodyToTell(t *testing.T) {
	t.Parallel()

	parent := publishedComment()
	repo := fullRepo().withComment(parent)
	svc := newService(t, repo, &fakePublisher{}, nil)
	ctx := context.Background()

	topLevel := comment.Comment{ID: 8, UserID: userID, PostID: postID, MessageID: 9100}
	_, ok, err := svc.ReplyNotice(ctx, topLevel)
	require.NoError(t, err)
	assert.False(t, ok, "a top-level comment answers nobody")

	selfReply := comment.Comment{
		ID: 8, UserID: parent.UserID, PostID: postID, MessageID: 9100, ReplyToCommentID: parent.ID,
	}
	_, ok, err = svc.ReplyNotice(ctx, selfReply)
	require.NoError(t, err)
	assert.False(t, ok, "answering yourself is not news")
}

func TestReplyNoticeParentGone(t *testing.T) {
	t.Parallel()

	svc := newService(t, fullRepo(), &fakePublisher{}, nil)

	reply := comment.Comment{ID: 8, UserID: userID, PostID: postID, MessageID: 9100, ReplyToCommentID: 7}
	_, _, err := svc.ReplyNotice(context.Background(), reply)
	require.ErrorIs(t, err, comment.ErrNotFound)
}
