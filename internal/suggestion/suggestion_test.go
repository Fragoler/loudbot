package suggestion_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"loudbot/internal/comment"
	"loudbot/internal/suggestion"
)

var errBoom = errors.New("boom")

const (
	author       = int64(777)
	other        = int64(888)
	maxText      = 10
	pendingLimit = 2
)

var now = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

type fakeRepo struct {
	posts  map[int64]suggestion.Suggestion
	drafts map[int64]suggestion.Draft
	nextID int64

	ensureErr error
	getErr    error
	createErr error
	updateErr error
	statusErr error
	countErr  error
	draftErr  error
	saveErr   error
	deleteErr error
}

func newRepo() *fakeRepo {
	return &fakeRepo{
		posts:  map[int64]suggestion.Suggestion{},
		drafts: map[int64]suggestion.Draft{},
	}
}

func (r *fakeRepo) withPost(s suggestion.Suggestion) *fakeRepo {
	if s.ID == 0 {
		r.nextID++
		s.ID = r.nextID
	}
	r.posts[s.ID] = s

	return r
}

func (r *fakeRepo) withDraft(d suggestion.Draft) *fakeRepo {
	r.drafts[d.UserID] = d

	return r
}

func (r *fakeRepo) EnsureUser(_ context.Context, id int64) (comment.User, error) {
	return comment.User{ID: id}, r.ensureErr
}

func (r *fakeRepo) CreateSuggestion(_ context.Context, s suggestion.Suggestion) (int64, error) {
	if r.createErr != nil {
		return 0, r.createErr
	}

	r.nextID++
	s.ID = r.nextID
	r.posts[s.ID] = s

	return s.ID, nil
}

func (r *fakeRepo) Suggestion(_ context.Context, id int64) (suggestion.Suggestion, error) {
	if r.getErr != nil {
		return suggestion.Suggestion{}, r.getErr
	}

	s, ok := r.posts[id]
	if !ok {
		return suggestion.Suggestion{}, suggestion.ErrNotFound
	}

	return s, nil
}

func (r *fakeRepo) SuggestionsByStatus(
	_ context.Context, userID int64, statuses ...suggestion.Status,
) ([]suggestion.Suggestion, error) {
	wanted := make(map[suggestion.Status]struct{}, len(statuses))
	for _, st := range statuses {
		wanted[st] = struct{}{}
	}

	var out []suggestion.Suggestion
	for _, s := range r.posts {
		if s.UserID != userID {
			continue
		}
		if _, ok := wanted[s.Status]; ok {
			out = append(out, s)
		}
	}

	return out, nil
}

func (r *fakeRepo) CountSuggestions(
	_ context.Context, userID int64, statuses ...suggestion.Status,
) (int, error) {
	if r.countErr != nil {
		return 0, r.countErr
	}

	wanted := make(map[suggestion.Status]struct{}, len(statuses))
	for _, st := range statuses {
		wanted[st] = struct{}{}
	}

	n := 0
	for _, s := range r.posts {
		if s.UserID != userID {
			continue
		}
		if _, ok := wanted[s.Status]; ok {
			n++
		}
	}

	return n, nil
}

func (r *fakeRepo) UpdateSuggestionText(
	_ context.Context, id int64, text string, media []comment.Media, at time.Time,
) error {
	if r.updateErr != nil {
		return r.updateErr
	}

	s := r.posts[id]
	s.Text = text
	s.Media = media
	s.EditedAt = &at
	r.posts[id] = s

	return nil
}

func (r *fakeRepo) SetSuggestionStatus(
	_ context.Context, id int64, status suggestion.Status, _ time.Time,
) error {
	if r.statusErr != nil {
		return r.statusErr
	}

	s := r.posts[id]
	s.Status = status
	r.posts[id] = s

	return nil
}

func (r *fakeRepo) SetModerationMessage(_ context.Context, id int64, messageID int) error {
	s := r.posts[id]
	s.ModerationMessageID = messageID
	r.posts[id] = s

	return nil
}

func (r *fakeRepo) SetChannelMessage(_ context.Context, id int64, messageID int) error {
	s := r.posts[id]
	s.ChannelMessageID = messageID
	r.posts[id] = s

	return nil
}

func (r *fakeRepo) SaveSuggestionDraft(_ context.Context, d suggestion.Draft) error {
	if r.saveErr != nil {
		return r.saveErr
	}

	r.drafts[d.UserID] = d

	return nil
}

func (r *fakeRepo) SuggestionDraft(_ context.Context, userID int64) (suggestion.Draft, error) {
	if r.draftErr != nil {
		return suggestion.Draft{}, r.draftErr
	}

	d, ok := r.drafts[userID]
	if !ok {
		return suggestion.Draft{}, suggestion.ErrNotFound
	}

	return d, nil
}

func (r *fakeRepo) DeleteSuggestionDraft(_ context.Context, userID int64) error {
	if r.deleteErr != nil {
		return r.deleteErr
	}

	delete(r.drafts, userID)

	return nil
}

func newService(repo *fakeRepo) *suggestion.Service {
	svc := suggestion.New(repo, suggestion.Options{MaxTextLen: maxText, PendingLimit: pendingLimit})
	svc.SetClock(func() time.Time { return now })

	return svc
}

func pending(userID int64) suggestion.Suggestion {
	return suggestion.Suggestion{
		UserID:              userID,
		Text:                "пост",
		Status:              suggestion.StatusPending,
		ModerationMessageID: 500,
		CreatedAt:           now,
	}
}

func TestBeginAndSubmitANewPost(t *testing.T) {
	t.Parallel()

	repo := newRepo()
	svc := newService(repo)
	ctx := context.Background()

	require.NoError(t, svc.Begin(ctx, author, 0))

	draft, err := svc.Waiting(ctx, author)
	require.NoError(t, err)
	assert.False(t, draft.Editing())

	sug, edited, err := svc.Submit(ctx, author, "  текст  ", nil)
	require.NoError(t, err)

	assert.False(t, edited)
	assert.NotZero(t, sug.ID)
	assert.Equal(t, "текст", sug.Text, "the stored text is trimmed")
	assert.Equal(t, suggestion.StatusPending, sug.Status)

	_, err = svc.Waiting(ctx, author)
	require.ErrorIs(t, err, suggestion.ErrNoDraft, "the bot stops waiting once the post arrives")
}

func TestSubmitWithoutBeingAsked(t *testing.T) {
	t.Parallel()

	_, _, err := newService(newRepo()).Submit(context.Background(), author, "текст", nil)
	require.ErrorIs(t, err, suggestion.ErrNoDraft)
}

func TestSubmitValidatesContent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		text    string
		media   []comment.Media
		wantErr error
	}{
		{name: "empty", text: "   ", wantErr: suggestion.ErrEmptyPost},
		{name: "too long", text: strings.Repeat("я", maxText+1), wantErr: suggestion.ErrTooLong},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := newRepo().withDraft(suggestion.Draft{UserID: author, CreatedAt: now})

			_, _, err := newService(repo).Submit(context.Background(), author, tc.text, tc.media)
			require.ErrorIs(t, err, tc.wantErr)
			assert.Empty(t, repo.posts, "a refused post is not recorded")
		})
	}
}

func TestSubmitAcceptsMediaWithoutText(t *testing.T) {
	t.Parallel()

	repo := newRepo().withDraft(suggestion.Draft{UserID: author, CreatedAt: now})

	sug, _, err := newService(repo).Submit(context.Background(), author, "",
		[]comment.Media{{Type: comment.MediaPhoto, FileID: "f1"}})
	require.NoError(t, err)
	require.Len(t, sug.Media, 1)
}

func TestEditKeepsTheCardAndMarksTheChange(t *testing.T) {
	t.Parallel()

	repo := newRepo().withPost(pending(author))
	svc := newService(repo)
	ctx := context.Background()

	require.NoError(t, svc.Begin(ctx, author, 1))

	sug, edited, err := svc.Submit(ctx, author, "новый", nil)
	require.NoError(t, err)

	assert.True(t, edited, "the transport rewrites the existing card rather than sending another")
	assert.Equal(t, int64(1), sug.ID, "an edit keeps its place in the queue")
	assert.Equal(t, 500, sug.ModerationMessageID)
	require.NotNil(t, sug.EditedAt)
	assert.Equal(t, "новый", repo.posts[1].Text)
}

func TestEditRefusesSomeoneElsesPost(t *testing.T) {
	t.Parallel()

	repo := newRepo().withPost(pending(other))

	err := newService(repo).Begin(context.Background(), author, 1)
	require.ErrorIs(t, err, suggestion.ErrNotYours,
		"an id arrives from the client, so ownership is checked here")
}

func TestEditRefusesADecidedPost(t *testing.T) {
	t.Parallel()

	decided := pending(author)
	decided.Status = suggestion.StatusApproved

	repo := newRepo().withPost(decided)

	err := newService(repo).Begin(context.Background(), author, 1)
	require.ErrorIs(t, err, suggestion.ErrNotPending)
}

func TestWithdraw(t *testing.T) {
	t.Parallel()

	repo := newRepo().withPost(pending(author))

	sug, err := newService(repo).Withdraw(context.Background(), author, 1)
	require.NoError(t, err)
	assert.Equal(t, suggestion.StatusWithdrawn, sug.Status)
	assert.Equal(t, 500, sug.ModerationMessageID, "the card is retired by the transport")
}

func TestWithdrawRefusesSomeoneElsesPost(t *testing.T) {
	t.Parallel()

	repo := newRepo().withPost(pending(other))

	_, err := newService(repo).Withdraw(context.Background(), author, 1)
	require.ErrorIs(t, err, suggestion.ErrNotYours)
}

func TestApproveAndDecline(t *testing.T) {
	t.Parallel()

	repo := newRepo().withPost(pending(author))
	svc := newService(repo)
	ctx := context.Background()

	sug, err := svc.Approve(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, suggestion.StatusApproved, sug.Status)

	// A second tap on a settled card changes nothing.
	_, err = svc.Approve(ctx, 1)
	require.ErrorIs(t, err, suggestion.ErrNotPending)

	_, err = svc.Decline(ctx, 1)
	require.ErrorIs(t, err, suggestion.ErrNotPending)

	require.NoError(t, svc.MarkPublished(ctx, 1, 4242))
	assert.Equal(t, 4242, repo.posts[1].ChannelMessageID)
	assert.Equal(t, suggestion.StatusPublished, repo.posts[1].Status)
}

func TestDecline(t *testing.T) {
	t.Parallel()

	repo := newRepo().withPost(pending(author))

	sug, err := newService(repo).Decline(context.Background(), 1)
	require.NoError(t, err)
	assert.Equal(t, suggestion.StatusDeclined, sug.Status)
}

func TestApproveAnUnknownPost(t *testing.T) {
	t.Parallel()

	_, err := newService(newRepo()).Approve(context.Background(), 99)
	require.ErrorIs(t, err, suggestion.ErrNotFound)
}

func TestMarkFailed(t *testing.T) {
	t.Parallel()

	approved := pending(author)
	approved.Status = suggestion.StatusApproved

	repo := newRepo().withPost(approved)

	require.NoError(t, newService(repo).MarkFailed(context.Background(), 1))
	assert.Equal(t, suggestion.StatusFailed, repo.posts[1].Status)
}

func TestLists(t *testing.T) {
	t.Parallel()

	live := pending(author)
	live.Status = suggestion.StatusPublished

	repo := newRepo().
		withPost(pending(author)).
		withPost(live).
		withPost(pending(other))

	svc := newService(repo)
	ctx := context.Background()

	waiting, err := svc.Pending(ctx, author)
	require.NoError(t, err)
	require.Len(t, waiting, 1, "one author's list must not show another's")

	published, err := svc.Published(ctx, author)
	require.NoError(t, err)
	require.Len(t, published, 1)
	assert.Equal(t, suggestion.StatusPublished, published[0].Status)
}

func TestAbandon(t *testing.T) {
	t.Parallel()

	repo := newRepo().withDraft(suggestion.Draft{UserID: author, CreatedAt: now})
	svc := newService(repo)
	ctx := context.Background()

	require.NoError(t, svc.Abandon(ctx, author))

	_, err := svc.Waiting(ctx, author)
	require.ErrorIs(t, err, suggestion.ErrNoDraft)
}

func TestRepositoryFailures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		run  func(*suggestion.Service, context.Context) error
		repo func() *fakeRepo
	}{
		{
			name: "ensure user fails on begin",
			repo: func() *fakeRepo { r := newRepo(); r.ensureErr = errBoom; return r },
			run:  func(s *suggestion.Service, ctx context.Context) error { return s.Begin(ctx, author, 0) },
		},
		{
			name: "saving the draft fails",
			repo: func() *fakeRepo { r := newRepo(); r.saveErr = errBoom; return r },
			run:  func(s *suggestion.Service, ctx context.Context) error { return s.Begin(ctx, author, 0) },
		},
		{
			name: "insert fails",
			repo: func() *fakeRepo {
				r := newRepo().withDraft(suggestion.Draft{UserID: author, CreatedAt: now})
				r.createErr = errBoom

				return r
			},
			run: func(s *suggestion.Service, ctx context.Context) error {
				_, _, err := s.Submit(ctx, author, "текст", nil)

				return err
			},
		},
		{
			name: "status update fails",
			repo: func() *fakeRepo { r := newRepo().withPost(pending(author)); r.statusErr = errBoom; return r },
			run: func(s *suggestion.Service, ctx context.Context) error {
				_, err := s.Approve(ctx, 1)

				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.ErrorIs(t, tc.run(newService(tc.repo()), context.Background()), errBoom)
		})
	}
}

func TestCallbackRoundTrip(t *testing.T) {
	t.Parallel()

	actions := []suggestion.Action{
		suggestion.ActionEdit,
		suggestion.ActionWithdraw,
		suggestion.ActionApprove,
		suggestion.ActionConfirm,
		suggestion.ActionAbort,
		suggestion.ActionDecline,
	}

	for _, want := range actions {
		data := suggestion.Callback(want, 42)
		require.True(t, suggestion.Owns(data), "the transport tells these apart by this prefix: %s", data)

		got, id, ok := suggestion.ParseCallback(data)
		require.True(t, ok, data)
		assert.Equal(t, want, got)
		assert.Equal(t, int64(42), id)
	}
}

func TestCallbackRejectsForeignData(t *testing.T) {
	t.Parallel()

	for _, data := range []string{"", "nick:Лис", "cancel", "sug:edit:", "sug:edit:abc", "sug:edit:0", "sug:what:1"} {
		_, _, ok := suggestion.ParseCallback(data)
		assert.False(t, ok, data)
	}

	assert.False(t, suggestion.Owns("nick:Лис"))
	assert.False(t, suggestion.Owns("cancel"))
}

func TestPendingLimit(t *testing.T) {
	t.Parallel()

	repo := newRepo()
	for range pendingLimit {
		repo.withPost(pending(author))
	}

	svc := newService(repo)
	ctx := context.Background()

	// Refused when the flow starts, so nobody writes a post only to be turned away.
	err := svc.Begin(ctx, author, 0)
	require.ErrorIs(t, err, suggestion.ErrTooManyPending)

	// And refused again at the moment a row would be added: a draft can sit open
	// while the queue fills up.
	repo.withDraft(suggestion.Draft{UserID: author, CreatedAt: now})
	_, _, err = svc.Submit(ctx, author, "текст", nil)
	require.ErrorIs(t, err, suggestion.ErrTooManyPending)

	// Another account is unaffected.
	require.NoError(t, svc.Begin(ctx, other, 0))
}

func TestPendingLimitCountsOnlyWhatWaits(t *testing.T) {
	t.Parallel()

	// Decided posts do not hold a slot, however many there are.
	repo := newRepo()
	for _, st := range []suggestion.Status{
		suggestion.StatusPublished,
		suggestion.StatusDeclined,
		suggestion.StatusWithdrawn,
		suggestion.StatusApproved,
	} {
		s := pending(author)
		s.Status = st
		repo.withPost(s)
	}

	require.NoError(t, newService(repo).Begin(context.Background(), author, 0))
}

func TestEditIsNotCappedByTheLimit(t *testing.T) {
	t.Parallel()

	repo := newRepo()
	for range pendingLimit {
		repo.withPost(pending(author))
	}

	// Rewriting an existing post adds nothing to the queue.
	require.NoError(t, newService(repo).Begin(context.Background(), author, 1))
}

func TestNoLimitWhenUnset(t *testing.T) {
	t.Parallel()

	repo := newRepo()
	for range 10 {
		repo.withPost(pending(author))
	}

	svc := suggestion.New(repo, suggestion.Options{MaxTextLen: maxText})
	require.NoError(t, svc.Begin(context.Background(), author, 0))
}
