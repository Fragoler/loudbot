// Package profile answers "what have I earned, and what can I wear": the
// achievements a person holds, the masks those unlock, and how much they have
// written. Achievements are granted by hand for now, so nothing here awards them.
package profile

import (
	"context"
	"fmt"

	"loudbot/internal/comment"
)

// Achievement is one entry of the known list. Code is the stable handle used
// when granting it; Title and Description are what the holder reads.
type Achievement struct {
	Code        string
	Title       string
	Description string
}

// Activity is how much of the channel one person has written.
type Activity struct {
	// Comments counts published top-level comments, Replies published answers.
	Comments int
	Replies  int
	// Posts is suggestions that reached the channel; PostsPending still await a
	// decision.
	Posts        int
	PostsPending int
}

// Profile is everything the bot shows a person about themselves.
type Profile struct {
	Activity     Activity
	Achievements []Achievement
	// Nicknames are the masks currently available, unlocked ones included.
	Nicknames []comment.Nickname
}

// Repository reads the parts a profile is assembled from.
type Repository interface {
	Achievements(ctx context.Context, userID int64) ([]Achievement, error)
	Activity(ctx context.Context, userID int64) (Activity, error)
	Nicknames(ctx context.Context, userID int64) ([]comment.Nickname, error)
	EnsureUser(ctx context.Context, userID int64) (comment.User, error)
}

type Service struct {
	repo Repository
}

func New(repo Repository) *Service {
	return &Service{repo: repo}
}

// Get assembles one person's profile. A user who has never written anything is
// not an error: they get an empty profile and the public masks.
func (s *Service) Get(ctx context.Context, userID int64) (Profile, error) {
	if _, err := s.repo.EnsureUser(ctx, userID); err != nil {
		return Profile{}, fmt.Errorf("ensure user: %w", err)
	}

	activity, err := s.repo.Activity(ctx, userID)
	if err != nil {
		return Profile{}, fmt.Errorf("activity: %w", err)
	}

	achievements, err := s.repo.Achievements(ctx, userID)
	if err != nil {
		return Profile{}, fmt.Errorf("achievements: %w", err)
	}

	nicknames, err := s.repo.Nicknames(ctx, userID)
	if err != nil {
		return Profile{}, fmt.Errorf("nicknames: %w", err)
	}

	return Profile{
		Activity:     activity,
		Achievements: achievements,
		Nicknames:    nicknames,
	}, nil
}
