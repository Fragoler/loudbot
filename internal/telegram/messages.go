package telegram

import (
	"errors"

	"loudbot/internal/comment"
	"loudbot/internal/config"
	"loudbot/internal/suggestion"
)

// userMessage maps a core error onto the line the author sees. An unmapped error
// is internal and must not leak its details.
func userMessage(m config.Messages, err error) string {
	if rejected, ok := comment.Rejected(err); ok {
		return rejected.Reason
	}

	if text, ok := suggestionMessage(m, err); ok {
		return text
	}

	switch {
	case errors.Is(err, comment.ErrBanned):
		return m.Errors.Banned
	case errors.Is(err, comment.ErrBadPayload):
		return m.Errors.BadPayload
	case errors.Is(err, comment.ErrUnknownPost):
		return m.Errors.UnknownPost
	case errors.Is(err, comment.ErrUnknownComment):
		return m.Errors.UnknownComment
	case errors.Is(err, comment.ErrNoNicknames):
		return m.Errors.NoNicknames
	case errors.Is(err, comment.ErrNoDraft):
		return m.Errors.NoDraft
	case errors.Is(err, comment.ErrDraftExpired):
		return m.Errors.DraftExpired
	case errors.Is(err, comment.ErrNicknameUnavailable):
		return m.Errors.Nickname
	case errors.Is(err, comment.ErrEmptyComment):
		return m.Errors.Empty
	case errors.Is(err, comment.ErrTextTooLong):
		return m.Errors.TooLong
	case errors.Is(err, comment.ErrNothingStaged):
		return m.Errors.NothingStaged
	default:
		return m.Errors.Internal
	}
}

// expected reports whether the error is a normal outcome of user input rather
// than a failure worth an error-level log line.
func expected(err error) bool {
	if _, ok := comment.Rejected(err); ok {
		return true
	}

	for _, sentinel := range []error{
		suggestion.ErrEmptyPost,
		suggestion.ErrTooLong,
		suggestion.ErrNotYours,
		suggestion.ErrNotPending,
		suggestion.ErrTooManyPending,
		suggestion.ErrNoDraft,
		suggestion.ErrNotFound,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}

	for _, sentinel := range []error{
		comment.ErrBanned,
		comment.ErrBadPayload,
		comment.ErrUnknownPost,
		comment.ErrUnknownComment,
		comment.ErrNoNicknames,
		comment.ErrNoDraft,
		comment.ErrDraftExpired,
		comment.ErrNicknameUnavailable,
		comment.ErrEmptyComment,
		comment.ErrTextTooLong,
		comment.ErrNothingStaged,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}

	return false
}
