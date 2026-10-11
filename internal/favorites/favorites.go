// Package favorites is the member's starred messages, artifacts and media
// items across projects, and their saved prompts (PLAN M13 cut 7).
// Adding a favorite validates the target through the same project access
// check as reading it; listing drops targets that are gone or no longer
// reachable.
package favorites

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

// Kinds and limits.
const (
	KindMessage  = "message"
	KindArtifact = "artifact"
	KindMedia    = "media"

	MaxFavorites    = 500
	MaxSavedPrompts = 50
	MaxPromptBytes  = 8 << 10
	snippetChars    = 240
)

// Errors with messages fit to show.
var (
	ErrInvalid  = errors.New("favorites: invalid")
	ErrNotFound = errors.New("favorites: not found")
)

// Store is what the service uses (*store.DB).
type Store interface {
	GetMessage(ctx context.Context, id uuid.UUID) (store.Message, error)
	GetConversation(ctx context.Context, id uuid.UUID) (store.Conversation, error)
	GetArtifact(ctx context.Context, id uuid.UUID) (store.Artifact, error)
	GetMediaJob(ctx context.Context, id uuid.UUID) (store.MediaJob, error)
	UserCanAccessProject(ctx context.Context, a store.UserCanAccessProjectParams) (bool, error)

	UpsertFavorite(ctx context.Context, a store.UpsertFavoriteParams) (store.Favorite, error)
	ListFavorites(ctx context.Context, userID uuid.UUID) ([]store.Favorite, error)
	CountFavorites(ctx context.Context, userID uuid.UUID) (int64, error)
	DeleteFavorite(ctx context.Context, a store.DeleteFavoriteParams) error

	CreateSavedPrompt(ctx context.Context, a store.CreateSavedPromptParams) (store.SavedPrompt, error)
	ListSavedPrompts(ctx context.Context, userID uuid.UUID) ([]store.SavedPrompt, error)
	CountSavedPrompts(ctx context.Context, userID uuid.UUID) (int64, error)
	UpdateSavedPrompt(ctx context.Context, a store.UpdateSavedPromptParams) (store.SavedPrompt, error)
	DeleteSavedPrompt(ctx context.Context, a store.DeleteSavedPromptParams) error
}

// Service owns favorites and saved prompts.
type Service struct{ DB Store }

// Favorite is one starred item as the page sees it.
type Favorite struct {
	ID             uuid.UUID     `json:"id"`
	Kind           string        `json:"kind"`
	RefID          uuid.UUID     `json:"ref_id"`
	Title          string        `json:"title"`
	Snippet        string        `json:"snippet"`
	ConversationID uuid.NullUUID `json:"conversation_id"`
	ProjectID      uuid.NullUUID `json:"project_id"`
	CreatedAt      time.Time     `json:"created_at"`
}

func view(r store.Favorite) Favorite {
	return Favorite{ID: r.ID, Kind: r.Kind, RefID: r.RefID, Title: r.Title, Snippet: r.Snippet, ConversationID: r.ConversationID, ProjectID: r.ProjectID, CreatedAt: r.CreatedAt}
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n]
		for len(s) > 0 && s[len(s)-1]&0xC0 == 0x80 {
			s = s[:len(s)-1]
		}
	}
	return s
}

func (s *Service) reach(ctx context.Context, userID, projectID uuid.UUID) bool {
	ok, err := s.DB.UserCanAccessProject(ctx, store.UserCanAccessProjectParams{ID: projectID, UserID: userID})
	return err == nil && ok
}

// Add stars an item. The target must exist and be in a project the member
// can reach; anything else is "not found", whichever it was.
func (s *Service) Add(ctx context.Context, userID uuid.UUID, kind string, ref uuid.UUID) (*Favorite, error) {
	var title, snippet string
	var conv, proj uuid.NullUUID
	switch kind {
	case KindMessage:
		m, err := s.DB.GetMessage(ctx, ref)
		if err != nil {
			return nil, ErrNotFound
		}
		c, err := s.DB.GetConversation(ctx, m.ConversationID)
		if err != nil || !s.reach(ctx, userID, c.ProjectID) {
			return nil, ErrNotFound
		}
		var parts []gateway.Part
		_ = json.Unmarshal(m.Parts, &parts)
		for _, p := range parts {
			if p.Kind == gateway.PartText && strings.TrimSpace(p.Text) != "" {
				snippet = clip(p.Text, snippetChars)
				break
			}
		}
		title = c.Title
		if title == "" {
			title = "Conversation"
		}
		conv, proj = store.NullUUID(c.ID), store.NullUUID(c.ProjectID)
	case KindArtifact:
		a, err := s.DB.GetArtifact(ctx, ref)
		if err != nil {
			return nil, ErrNotFound
		}
		c, err := s.DB.GetConversation(ctx, a.ConversationID)
		if err != nil || !s.reach(ctx, userID, c.ProjectID) {
			return nil, ErrNotFound
		}
		title = a.Title
		if title == "" {
			title = "Untitled " + a.Kind
		}
		snippet = fmt.Sprintf("%s, version %d", a.Kind, a.CurrentVersion)
		conv, proj = store.NullUUID(c.ID), store.NullUUID(c.ProjectID)
	case KindMedia:
		j, err := s.DB.GetMediaJob(ctx, ref)
		if err != nil || !s.reach(ctx, userID, j.ProjectID) {
			return nil, ErrNotFound
		}
		var in struct {
			Prompt string `json:"prompt"`
		}
		_ = json.Unmarshal(j.Inputs, &in)
		title = clip(in.Prompt, 80)
		if title == "" {
			title = j.Kind
		}
		snippet = j.Kind
		conv, proj = j.ConversationID, store.NullUUID(j.ProjectID)
	default:
		return nil, fmt.Errorf("%w: kind is message, artifact or media", ErrInvalid)
	}
	if n, err := s.DB.CountFavorites(ctx, userID); err == nil && n >= MaxFavorites {
		return nil, fmt.Errorf("%w: you have %d favorites, the most ws keeps; remove some first", ErrInvalid, MaxFavorites)
	}
	row, err := s.DB.UpsertFavorite(ctx, store.UpsertFavoriteParams{UserID: userID, Kind: kind, RefID: ref, Title: clip(title, 120), Snippet: snippet, ConversationID: conv, ProjectID: proj})
	if err != nil {
		return nil, err
	}
	v := view(row)
	return &v, nil
}

// Remove un-stars an item; removing one that is not starred is not an error.
func (s *Service) Remove(ctx context.Context, userID uuid.UUID, kind string, ref uuid.UUID) error {
	return s.DB.DeleteFavorite(ctx, store.DeleteFavoriteParams{UserID: userID, Kind: kind, RefID: ref})
}

// List returns the member's reachable favorites, newest first.
func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]Favorite, error) {
	rows, err := s.DB.ListFavorites(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]Favorite, len(rows))
	for i, r := range rows {
		out[i] = view(r)
	}
	return out, nil
}

// ---- saved prompts ----

// SavedPrompt is one reusable text.
type SavedPrompt struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	Text string    `json:"text"`
}

func pview(r store.SavedPrompt) SavedPrompt { return SavedPrompt{ID: r.ID, Name: r.Name, Text: r.Text} }

func checkPrompt(name, text string) (string, string, error) {
	name, text = strings.TrimSpace(name), strings.TrimSpace(text)
	if name == "" || len(name) > 60 {
		return "", "", fmt.Errorf("%w: a name is 1 to 60 characters", ErrInvalid)
	}
	if strings.ContainsAny(name, "\n\r") {
		return "", "", fmt.Errorf("%w: a name is one line", ErrInvalid)
	}
	if text == "" || len(text) > MaxPromptBytes {
		return "", "", fmt.Errorf("%w: the text is 1 to %d bytes", ErrInvalid, MaxPromptBytes)
	}
	return name, text, nil
}

// SavePrompt stores a prompt.
func (s *Service) SavePrompt(ctx context.Context, userID uuid.UUID, name, text string) (*SavedPrompt, error) {
	name, text, err := checkPrompt(name, text)
	if err != nil {
		return nil, err
	}
	if n, err := s.DB.CountSavedPrompts(ctx, userID); err == nil && n >= MaxSavedPrompts {
		return nil, fmt.Errorf("%w: you have %d saved prompts, the most ws keeps", ErrInvalid, MaxSavedPrompts)
	}
	row, err := s.DB.CreateSavedPrompt(ctx, store.CreateSavedPromptParams{UserID: userID, Name: name, Text: text})
	if err != nil {
		return nil, err
	}
	v := pview(row)
	return &v, nil
}

// UpdatePrompt edits one.
func (s *Service) UpdatePrompt(ctx context.Context, userID, id uuid.UUID, name, text string) (*SavedPrompt, error) {
	name, text, err := checkPrompt(name, text)
	if err != nil {
		return nil, err
	}
	row, err := s.DB.UpdateSavedPrompt(ctx, store.UpdateSavedPromptParams{ID: id, UserID: userID, Name: name, Text: text})
	if err != nil {
		return nil, ErrNotFound
	}
	v := pview(row)
	return &v, nil
}

// DeletePrompt removes one.
func (s *Service) DeletePrompt(ctx context.Context, userID, id uuid.UUID) error {
	return s.DB.DeleteSavedPrompt(ctx, store.DeleteSavedPromptParams{ID: id, UserID: userID})
}

// ListPrompts returns the member's saved prompts by name.
func (s *Service) ListPrompts(ctx context.Context, userID uuid.UUID) ([]SavedPrompt, error) {
	rows, err := s.DB.ListSavedPrompts(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]SavedPrompt, len(rows))
	for i, r := range rows {
		out[i] = pview(r)
	}
	return out, nil
}
