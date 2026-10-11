// Package notebooks is the research notebook (PLAN M13 cut 6): sources a
// member collects (a file, a fetched page, pasted text, another
// conversation), notes about them, and one chat whose context is the
// sources. Sources are chunked and embedded; a notebook turn gets the
// nearest chunks with their source ids so replies can cite them, and the
// model keeps a summary and an open-questions note up to date with the
// notebook_note tool. Everything is per member.
package notebooks

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"

	"github.com/jking323/ws/internal/agents"
	"github.com/jking323/ws/internal/docread"
	"github.com/jking323/ws/internal/documents"
	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
	"github.com/jking323/ws/internal/store/blob"
)

// Limits and kinds.
const (
	KindDocument     = "document"
	KindPage         = "page"
	KindText         = "text"
	KindConversation = "conversation"

	NoteSummary       = "summary"
	NoteOpenQuestions = "open_questions"
	NoteNote          = "note"

	MaxNotebooks = 20
	MaxSources   = 30
	MaxNotes     = 200
	MaxNoteChars = 20000
	MaxTextChars = 500000
	// ContextChars bounds what a turn's notebook block adds to the prompt.
	ContextChars = 9000
	embedBatch   = 32
	EmbedDims    = 768
)

// Errors with messages fit to show.
var (
	ErrInvalid  = errors.New("notebooks: invalid")
	ErrNotFound = errors.New("notebooks: not found")
)

// Store is what the service uses (*store.DB).
type Store interface {
	CreateNotebook(ctx context.Context, a store.CreateNotebookParams) (store.Notebook, error)
	GetNotebook(ctx context.Context, a store.GetNotebookParams) (store.Notebook, error)
	ListNotebooks(ctx context.Context, userID uuid.UUID) ([]store.ListNotebooksRow, error)
	CountNotebooks(ctx context.Context, userID uuid.UUID) (int64, error)
	RenameNotebook(ctx context.Context, a store.RenameNotebookParams) (store.Notebook, error)
	TouchNotebook(ctx context.Context, id uuid.UUID) error
	SetNotebookConversation(ctx context.Context, a store.SetNotebookConversationParams) error
	DeleteNotebook(ctx context.Context, a store.DeleteNotebookParams) error
	GetNotebookByConversation(ctx context.Context, a store.GetNotebookByConversationParams) (store.Notebook, error)
	CreateNotebookSource(ctx context.Context, a store.CreateNotebookSourceParams) (store.NotebookSource, error)
	ListNotebookSources(ctx context.Context, notebookID uuid.UUID) ([]store.NotebookSource, error)
	CountNotebookSources(ctx context.Context, notebookID uuid.UUID) (int64, error)
	GetNotebookSource(ctx context.Context, a store.GetNotebookSourceParams) (store.NotebookSource, error)
	DeleteNotebookSource(ctx context.Context, a store.DeleteNotebookSourceParams) error
	InsertNotebookChunk(ctx context.Context, a store.InsertNotebookChunkParams) error
	ListNotebookChunks(ctx context.Context, sourceID uuid.UUID) ([]store.ListNotebookChunksRow, error)
	ListNotebookChunksFirst(ctx context.Context, notebookID uuid.UUID) ([]store.ListNotebookChunksFirstRow, error)
	SearchNotebookChunks(ctx context.Context, a store.SearchNotebookChunksParams) ([]store.SearchNotebookChunksRow, error)
	ListNotebookNotes(ctx context.Context, notebookID uuid.UUID) ([]store.NotebookNote, error)
	CountNotebookNotes(ctx context.Context, notebookID uuid.UUID) (int64, error)
	GetNotebookNote(ctx context.Context, a store.GetNotebookNoteParams) (store.NotebookNote, error)
	CreateNotebookNote(ctx context.Context, a store.CreateNotebookNoteParams) (store.NotebookNote, error)
	UpsertNotebookSingletonNote(ctx context.Context, a store.UpsertNotebookSingletonNoteParams) (store.NotebookNote, error)
	UpdateNotebookNote(ctx context.Context, a store.UpdateNotebookNoteParams) (store.NotebookNote, error)
	DeleteNotebookNote(ctx context.Context, a store.DeleteNotebookNoteParams) error

	CreateAttachment(ctx context.Context, a store.CreateAttachmentParams) (store.Attachment, error)
	GetConversation(ctx context.Context, id uuid.UUID) (store.Conversation, error)
	ListMessages(ctx context.Context, conversationID uuid.UUID) ([]store.Message, error)
	ListProjectsForUser(ctx context.Context, userID uuid.UUID) ([]store.Project, error)
	CreateProject(ctx context.Context, a store.CreateProjectParams) (store.Project, error)
	CreateConversation(ctx context.Context, a store.CreateConversationParams) (store.Conversation, error)
}

// Embedder is the gateway's embedding call.
type Embedder interface {
	Embed(ctx context.Context, req *gateway.EmbedRequest) (*gateway.EmbedResponse, error)
}

// Service owns notebooks.
type Service struct {
	DB     Store
	Blobs  blob.Store
	Reader *docread.Reader
	GW     Embedder
	// Fetch returns the text of a public page (the guarded fetch behind
	// web_fetch); nil refuses URL sources.
	Fetch func(ctx context.Context, url string) (string, error)
	Log   *slog.Logger
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Notebook, Source, Note and Detail are what the page sees.
type Notebook struct {
	ID             uuid.UUID     `json:"id"`
	Name           string        `json:"name"`
	ConversationID uuid.NullUUID `json:"conversation_id"`
	Sources        int           `json:"sources"`
	UpdatedAt      string        `json:"updated_at"`
}

type Source struct {
	ID                 uuid.UUID     `json:"id"`
	Kind               string        `json:"kind"`
	Title              string        `json:"title"`
	URL                string        `json:"url,omitempty"`
	AttachmentID       uuid.NullUUID `json:"attachment_id"`
	FromConversationID uuid.NullUUID `json:"from_conversation_id"`
	Pages              int           `json:"pages"`
	Chars              int           `json:"chars"`
}

type Note struct {
	ID        uuid.UUID `json:"id"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Author    string    `json:"author"`
	UpdatedAt string    `json:"updated_at"`
}

type Detail struct {
	Notebook Notebook `json:"notebook"`
	Sources  []Source `json:"sources"`
	Notes    []Note   `json:"notes"`
}

func srcView(r store.NotebookSource) Source {
	return Source{ID: r.ID, Kind: r.Kind, Title: r.Title, URL: r.Url, AttachmentID: r.AttachmentID, FromConversationID: r.FromConversationID, Pages: int(r.Pages), Chars: int(r.Chars)}
}

func noteView(r store.NotebookNote) Note {
	return Note{ID: r.ID, Kind: r.Kind, Title: r.Title, Body: r.Body, Author: r.Author, UpdatedAt: r.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")}
}

// Create makes an empty notebook.
func (s *Service) Create(ctx context.Context, userID uuid.UUID, name string) (*Notebook, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: a notebook needs a name", ErrInvalid)
	}
	if len(name) > 120 {
		name = name[:120]
	}
	if n, err := s.DB.CountNotebooks(ctx, userID); err == nil && n >= MaxNotebooks {
		return nil, fmt.Errorf("%w: you have %d notebooks, the most ws keeps", ErrInvalid, MaxNotebooks)
	}
	row, err := s.DB.CreateNotebook(ctx, store.CreateNotebookParams{UserID: userID, Name: name})
	if err != nil {
		return nil, err
	}
	return &Notebook{ID: row.ID, Name: row.Name, ConversationID: row.ConversationID, UpdatedAt: row.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")}, nil
}

// List returns the member's notebooks, most recently used first.
func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]Notebook, error) {
	rows, err := s.DB.ListNotebooks(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]Notebook, len(rows))
	for i, r := range rows {
		out[i] = Notebook{ID: r.ID, Name: r.Name, ConversationID: r.ConversationID, Sources: int(r.SourceCount), UpdatedAt: r.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")}
	}
	return out, nil
}

func (s *Service) own(ctx context.Context, userID, id uuid.UUID) (store.Notebook, error) {
	nb, err := s.DB.GetNotebook(ctx, store.GetNotebookParams{ID: id, UserID: userID})
	if err != nil {
		return store.Notebook{}, ErrNotFound
	}
	return nb, nil
}

// Get returns a notebook with its sources and notes.
func (s *Service) Get(ctx context.Context, userID, id uuid.UUID) (*Detail, error) {
	nb, err := s.own(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	srcs, err := s.DB.ListNotebookSources(ctx, id)
	if err != nil {
		return nil, err
	}
	notes, err := s.DB.ListNotebookNotes(ctx, id)
	if err != nil {
		return nil, err
	}
	d := &Detail{Notebook: Notebook{ID: nb.ID, Name: nb.Name, ConversationID: nb.ConversationID, Sources: len(srcs), UpdatedAt: nb.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")},
		Sources: []Source{}, Notes: []Note{}}
	for _, r := range srcs {
		d.Sources = append(d.Sources, srcView(r))
	}
	for _, r := range notes {
		d.Notes = append(d.Notes, noteView(r))
	}
	return d, nil
}

// Rename changes a notebook's name.
func (s *Service) Rename(ctx context.Context, userID, id uuid.UUID, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 120 {
		return fmt.Errorf("%w: a name is 1 to 120 characters", ErrInvalid)
	}
	if _, err := s.DB.RenameNotebook(ctx, store.RenameNotebookParams{ID: id, UserID: userID, Name: name}); err != nil {
		return ErrNotFound
	}
	return nil
}

// Delete removes a notebook with its sources and notes; its chat remains
// as an ordinary conversation.
func (s *Service) Delete(ctx context.Context, userID, id uuid.UUID) error {
	if _, err := s.own(ctx, userID, id); err != nil {
		return err
	}
	return s.DB.DeleteNotebook(ctx, store.DeleteNotebookParams{ID: id, UserID: userID})
}

// ---- sources ----

func (s *Service) sourceRoom(ctx context.Context, nbID uuid.UUID) error {
	if n, err := s.DB.CountNotebookSources(ctx, nbID); err == nil && n >= MaxSources {
		return fmt.Errorf("%w: a notebook holds at most %d sources", ErrInvalid, MaxSources)
	}
	return nil
}

// AddFile reads an uploaded file into a source and keeps the original.
func (s *Service) AddFile(ctx context.Context, userID, nbID uuid.UUID, mime, name string, data []byte) (*Source, error) {
	if _, err := s.own(ctx, userID, nbID); err != nil {
		return nil, err
	}
	if err := s.sourceRoom(ctx, nbID); err != nil {
		return nil, err
	}
	doc, err := s.Reader.Extract(ctx, mime, name, data)
	if err != nil {
		return nil, readErr(err)
	}
	var att uuid.NullUUID
	if s.Blobs != nil {
		if key, perr := blob.PutBytes(ctx, s.Blobs, data); perr == nil {
			sum := sha256.Sum256(data)
			if a, aerr := s.DB.CreateAttachment(ctx, store.CreateAttachmentParams{UserID: userID, BlobKey: key, Mime: mime, Bytes: int64(len(data)), Sha256: sum[:], Filename: name}); aerr == nil {
				att = store.NullUUID(a.ID)
			}
		}
	}
	title := strings.TrimSpace(name)
	if title == "" {
		title = "Document"
	}
	return s.addSource(ctx, nbID, KindDocument, title, "", att, uuid.NullUUID{}, doc)
}

// AddText adds pasted text.
func (s *Service) AddText(ctx context.Context, userID, nbID uuid.UUID, title, text string) (*Source, error) {
	if _, err := s.own(ctx, userID, nbID); err != nil {
		return nil, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("%w: there is no text", ErrInvalid)
	}
	if len(text) > MaxTextChars {
		return nil, fmt.Errorf("%w: text is over %d characters", ErrInvalid, MaxTextChars)
	}
	if err := s.sourceRoom(ctx, nbID); err != nil {
		return nil, err
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = firstLine(text, 60)
	}
	doc, err := s.Reader.Extract(ctx, "text/plain", "text.txt", []byte(text))
	if err != nil {
		return nil, readErr(err)
	}
	return s.addSource(ctx, nbID, KindText, title, "", uuid.NullUUID{}, uuid.NullUUID{}, doc)
}

// AddURL fetches a public page and adds its text.
func (s *Service) AddURL(ctx context.Context, userID, nbID uuid.UUID, rawURL string) (*Source, error) {
	if _, err := s.own(ctx, userID, nbID); err != nil {
		return nil, err
	}
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%w: the address must be an http or https URL", ErrInvalid)
	}
	if s.Fetch == nil {
		return nil, fmt.Errorf("%w: fetching pages is not available on this server", ErrInvalid)
	}
	if err := s.sourceRoom(ctx, nbID); err != nil {
		return nil, err
	}
	text, err := s.Fetch(ctx, u.String())
	if err != nil {
		return nil, fmt.Errorf("%w: could not read that page: %v", ErrInvalid, err)
	}
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("%w: the page has no readable text", ErrInvalid)
	}
	doc, err := s.Reader.Extract(ctx, "text/plain", "page.txt", []byte(text))
	if err != nil {
		return nil, readErr(err)
	}
	return s.addSource(ctx, nbID, KindPage, u.Host+u.Path, u.String(), uuid.NullUUID{}, uuid.NullUUID{}, doc)
}

// AddConversation adds the transcript of one of the member's conversations.
func (s *Service) AddConversation(ctx context.Context, userID, nbID, convID uuid.UUID) (*Source, error) {
	if _, err := s.own(ctx, userID, nbID); err != nil {
		return nil, err
	}
	conv, err := s.DB.GetConversation(ctx, convID)
	if err != nil || conv.UserID != userID {
		return nil, fmt.Errorf("%w: that conversation is not yours", ErrInvalid)
	}
	if err := s.sourceRoom(ctx, nbID); err != nil {
		return nil, err
	}
	rows, err := s.DB.ListMessages(ctx, convID)
	if err != nil {
		return nil, err
	}
	text := agents.Transcript(rows, 200000)
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("%w: that conversation is empty", ErrInvalid)
	}
	doc, err := s.Reader.Extract(ctx, "text/plain", "conversation.txt", []byte(text))
	if err != nil {
		return nil, readErr(err)
	}
	title := conv.Title
	if title == "" {
		title = "Conversation"
	}
	return s.addSource(ctx, nbID, KindConversation, title, "", uuid.NullUUID{}, store.NullUUID(convID), doc)
}

func readErr(err error) error {
	switch {
	case errors.Is(err, docread.ErrNeedsOCR):
		return fmt.Errorf("%w: that file is a scan or an image and this server has no OCR service configured", ErrInvalid)
	case errors.Is(err, docread.ErrTooBig):
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	case errors.Is(err, docread.ErrUnsupported):
		return fmt.Errorf("%w: this kind of file is not supported", ErrInvalid)
	}
	return fmt.Errorf("%w: the file may be damaged or protected by a password", ErrInvalid)
}

func firstLine(s string, n int) string {
	s = strings.TrimSpace(strings.SplitN(strings.TrimSpace(s), "\n", 2)[0])
	if len(s) > n {
		s = s[:n]
	}
	return s
}

func (s *Service) addSource(ctx context.Context, nbID uuid.UUID, kind, title, rawURL string, att, conv uuid.NullUUID, doc *docread.Doc) (*Source, error) {
	row, err := s.DB.CreateNotebookSource(ctx, store.CreateNotebookSourceParams{
		NotebookID: nbID, Kind: kind, Title: title, Url: rawURL, AttachmentID: att, FromConversationID: conv,
		Pages: int32(len(doc.Pages)), Chars: int32(doc.Chars()),
	})
	if err != nil {
		return nil, err
	}
	chunks := documents.ChunkDoc(doc)
	vecs := make([]*pgvector.Vector, len(chunks))
	if s.GW != nil {
		for i := 0; i < len(chunks); i += embedBatch {
			j := min(i+embedBatch, len(chunks))
			texts := make([]string, 0, j-i)
			for _, c := range chunks[i:j] {
				texts = append(texts, c.Text)
			}
			resp, err := s.GW.Embed(ctx, &gateway.EmbedRequest{Model: "auto", Inputs: texts, Metadata: gateway.Metadata{}})
			if err != nil || len(resp.Vectors) != len(texts) {
				s.log().Warn("notebooks: embedding failed, storing without vectors", "err", err)
				break
			}
			for k, v := range resp.Vectors {
				if len(v) == EmbedDims {
					pv := pgvector.NewVector(v)
					vecs[i+k] = &pv
				}
			}
		}
	}
	for i, c := range chunks {
		if err := s.DB.InsertNotebookChunk(ctx, store.InsertNotebookChunkParams{SourceID: row.ID, Seq: int32(i), Page: int32(c.Page), Content: c.Text, Embedding: vecs[i]}); err != nil {
			return nil, err
		}
	}
	_ = s.DB.TouchNotebook(ctx, nbID)
	v := srcView(row)
	return &v, nil
}

// SourceText is one source with its text, for the source viewer.
type SourceText struct {
	Source Source       `json:"source"`
	Chunks []SourcePage `json:"pages"`
}

// SourcePage is the text of one page of a source.
type SourcePage struct {
	Page int    `json:"page"`
	Text string `json:"text"`
}

// Source returns a source with its text grouped by page.
func (s *Service) Source(ctx context.Context, userID, nbID, srcID uuid.UUID) (*SourceText, error) {
	if _, err := s.own(ctx, userID, nbID); err != nil {
		return nil, err
	}
	row, err := s.DB.GetNotebookSource(ctx, store.GetNotebookSourceParams{ID: srcID, NotebookID: nbID})
	if err != nil {
		return nil, ErrNotFound
	}
	chunks, err := s.DB.ListNotebookChunks(ctx, srcID)
	if err != nil {
		return nil, err
	}
	out := &SourceText{Source: srcView(row), Chunks: []SourcePage{}}
	for _, c := range chunks {
		if n := len(out.Chunks); n > 0 && out.Chunks[n-1].Page == int(c.Page) {
			out.Chunks[n-1].Text += "\n" + c.Content
			continue
		}
		out.Chunks = append(out.Chunks, SourcePage{Page: int(c.Page), Text: c.Content})
	}
	return out, nil
}

// DeleteSource removes a source and its chunks.
func (s *Service) DeleteSource(ctx context.Context, userID, nbID, srcID uuid.UUID) error {
	if _, err := s.own(ctx, userID, nbID); err != nil {
		return err
	}
	if _, err := s.DB.GetNotebookSource(ctx, store.GetNotebookSourceParams{ID: srcID, NotebookID: nbID}); err != nil {
		return ErrNotFound
	}
	return s.DB.DeleteNotebookSource(ctx, store.DeleteNotebookSourceParams{ID: srcID, NotebookID: nbID})
}

// ---- notes ----

// AddNote writes one of the member's own notes.
func (s *Service) AddNote(ctx context.Context, userID, nbID uuid.UUID, title, body string) (*Note, error) {
	if _, err := s.own(ctx, userID, nbID); err != nil {
		return nil, err
	}
	title, body = strings.TrimSpace(title), strings.TrimSpace(body)
	if body == "" || len(body) > MaxNoteChars {
		return nil, fmt.Errorf("%w: a note is 1 to %d characters", ErrInvalid, MaxNoteChars)
	}
	if n, err := s.DB.CountNotebookNotes(ctx, nbID); err == nil && n >= MaxNotes {
		return nil, fmt.Errorf("%w: a notebook holds at most %d notes", ErrInvalid, MaxNotes)
	}
	row, err := s.DB.CreateNotebookNote(ctx, store.CreateNotebookNoteParams{NotebookID: nbID, Kind: NoteNote, Title: clip(title, 120), Body: body, Author: "user"})
	if err != nil {
		return nil, err
	}
	_ = s.DB.TouchNotebook(ctx, nbID)
	v := noteView(row)
	return &v, nil
}

// UpdateNote edits a note; an edit makes the person its author.
func (s *Service) UpdateNote(ctx context.Context, userID, nbID, noteID uuid.UUID, title, body string) (*Note, error) {
	if _, err := s.own(ctx, userID, nbID); err != nil {
		return nil, err
	}
	body = strings.TrimSpace(body)
	if body == "" || len(body) > MaxNoteChars {
		return nil, fmt.Errorf("%w: a note is 1 to %d characters", ErrInvalid, MaxNoteChars)
	}
	row, err := s.DB.UpdateNotebookNote(ctx, store.UpdateNotebookNoteParams{ID: noteID, NotebookID: nbID, Title: clip(strings.TrimSpace(title), 120), Body: body})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	v := noteView(row)
	return &v, nil
}

// DeleteNote removes a note.
func (s *Service) DeleteNote(ctx context.Context, userID, nbID, noteID uuid.UUID) error {
	if _, err := s.own(ctx, userID, nbID); err != nil {
		return err
	}
	if _, err := s.DB.GetNotebookNote(ctx, store.GetNotebookNoteParams{ID: noteID, NotebookID: nbID}); err != nil {
		return ErrNotFound
	}
	return s.DB.DeleteNotebookNote(ctx, store.DeleteNotebookNoteParams{ID: noteID, NotebookID: nbID})
}

// SetModelNote is what the notebook_note tool writes: the summary or the
// open-questions note is replaced; anything else is a new note by the model.
func (s *Service) SetModelNote(ctx context.Context, nbID uuid.UUID, kind, title, body string) (*Note, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, fmt.Errorf("%w: the note is empty", ErrInvalid)
	}
	if len(body) > MaxNoteChars {
		body = body[:MaxNoteChars]
	}
	var row store.NotebookNote
	var err error
	switch kind {
	case NoteSummary, NoteOpenQuestions:
		row, err = s.DB.UpsertNotebookSingletonNote(ctx, store.UpsertNotebookSingletonNoteParams{NotebookID: nbID, Kind: kind, Title: clip(title, 120), Body: body, Author: "model"})
	case NoteNote, "":
		if n, cerr := s.DB.CountNotebookNotes(ctx, nbID); cerr == nil && n >= MaxNotes {
			return nil, fmt.Errorf("%w: the notebook is full of notes", ErrInvalid)
		}
		row, err = s.DB.CreateNotebookNote(ctx, store.CreateNotebookNoteParams{NotebookID: nbID, Kind: NoteNote, Title: clip(title, 120), Body: body, Author: "model"})
	default:
		return nil, fmt.Errorf("%w: kind is summary, open_questions or note", ErrInvalid)
	}
	if err != nil {
		return nil, err
	}
	_ = s.DB.TouchNotebook(ctx, nbID)
	v := noteView(row)
	return &v, nil
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---- the chat ----

// Chat returns the notebook's conversation, creating it in the member's
// first chat project (or a new "Notebooks" one) on first use. The
// conversation's settings carry the notebook id, which the chat turn reads.
func (s *Service) Chat(ctx context.Context, userID, nbID uuid.UUID) (uuid.UUID, error) {
	nb, err := s.own(ctx, userID, nbID)
	if err != nil {
		return uuid.Nil, err
	}
	if nb.ConversationID.Valid {
		if c, err := s.DB.GetConversation(ctx, nb.ConversationID.UUID); err == nil && c.UserID == userID {
			return c.ID, nil
		}
	}
	projects, err := s.DB.ListProjectsForUser(ctx, userID)
	if err != nil {
		return uuid.Nil, err
	}
	var proj uuid.UUID
	for _, p := range projects {
		if p.Kind == "chat" && p.OwnerID == userID {
			proj = p.ID
			break
		}
	}
	if proj == uuid.Nil {
		p, err := s.DB.CreateProject(ctx, store.CreateProjectParams{OwnerID: userID, Name: "Notebooks", Kind: "chat", Settings: json.RawMessage("{}")})
		if err != nil {
			return uuid.Nil, err
		}
		proj = p.ID
	}
	settings, _ := json.Marshal(map[string]string{"notebook_id": nbID.String()})
	c, err := s.DB.CreateConversation(ctx, store.CreateConversationParams{
		ProjectID: proj, UserID: userID, Title: nb.Name, Mode: "chat", ModelSelector: "auto", Settings: settings,
	})
	if err != nil {
		return uuid.Nil, err
	}
	if err := s.DB.SetNotebookConversation(ctx, store.SetNotebookConversationParams{ID: nbID, ConversationID: store.NullUUID(c.ID)}); err != nil {
		return uuid.Nil, err
	}
	return c.ID, nil
}

// Context builds the notebook block for a turn of the notebook's chat:
// the instructions, the sources with their ids, the two notes, and the
// chunks nearest the message, each marked with its source id and page.
// notebookID comes from the conversation's settings; a notebook that is
// not the caller's yields nothing.
func (s *Service) Context(ctx context.Context, userID, nbID uuid.UUID, query string) (string, error) {
	nb, err := s.own(ctx, userID, nbID)
	if err != nil {
		return "", nil
	}
	srcs, err := s.DB.ListNotebookSources(ctx, nbID)
	if err != nil {
		return "", err
	}
	notes, _ := s.DB.ListNotebookNotes(ctx, nbID)
	titles := map[uuid.UUID]string{}
	var b strings.Builder
	fmt.Fprintf(&b, "You are working in the research notebook %q. Answer from its sources and cite them: write [short label](src: <source id> p.<page>) wherever a claim rests on a source, using the ids below. You may add a short explanation for a term as [word](tip: one sentence). When the picture changes, call notebook_note to keep the summary and the open-questions notes current. Say plainly when the sources do not answer something.\n", nb.Name)
	if len(srcs) == 0 {
		b.WriteString("\nThe notebook has no sources yet.\n")
	} else {
		b.WriteString("\nSources:\n")
		for _, r := range srcs {
			titles[r.ID] = r.Title
			fmt.Fprintf(&b, "- %s: %s (%s, %d page(s))\n", r.ID, r.Title, r.Kind, r.Pages)
		}
	}
	for _, n := range notes {
		switch n.Kind {
		case NoteSummary:
			fmt.Fprintf(&b, "\nCurrent summary note:\n%s\n", clip(n.Body, 1500))
		case NoteOpenQuestions:
			fmt.Fprintf(&b, "\nCurrent open questions:\n%s\n", clip(n.Body, 1000))
		}
	}
	type hit struct {
		src  uuid.UUID
		page int32
		text string
	}
	var hits []hit
	if q := strings.TrimSpace(query); q != "" && s.GW != nil && len(srcs) > 0 {
		resp, err := s.GW.Embed(ctx, &gateway.EmbedRequest{Model: "auto", Inputs: []string{q}, Metadata: gateway.Metadata{UserID: userID.String()}})
		if err == nil && len(resp.Vectors) == 1 && len(resp.Vectors[0]) == EmbedDims {
			rows, serr := s.DB.SearchNotebookChunks(ctx, store.SearchNotebookChunksParams{QueryEmbedding: pgvector.NewVector(resp.Vectors[0]), NotebookID: nbID, MaxRows: 8})
			if serr != nil {
				return "", serr
			}
			for _, r := range rows {
				hits = append(hits, hit{r.SourceID, r.Page, r.Content})
			}
		}
	}
	if len(hits) == 0 && len(srcs) > 0 {
		// No embedding model or no match: the opening of each source, so the
		// model has something real to cite.
		rows, err := s.DB.ListNotebookChunksFirst(ctx, nbID)
		if err == nil {
			for _, r := range rows {
				hits = append(hits, hit{r.SourceID, r.Page, r.Content})
			}
		}
	}
	if len(hits) > 0 {
		b.WriteString("\nPassages most relevant to the message:\n")
		for _, h := range hits {
			piece := fmt.Sprintf("[source %s, %q, p.%d]\n%s\n\n", h.src, titles[h.src], h.page, strings.TrimSpace(h.text))
			if b.Len()+len(piece) > ContextChars {
				break
			}
			b.WriteString(piece)
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// NotebookForConversation reports the notebook a conversation's settings
// name, when it is the member's.
func (s *Service) NotebookForConversation(ctx context.Context, userID uuid.UUID, settings json.RawMessage) (uuid.UUID, bool) {
	var st struct {
		NotebookID string `json:"notebook_id"`
	}
	if len(settings) == 0 || json.Unmarshal(settings, &st) != nil || st.NotebookID == "" {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(st.NotebookID)
	if err != nil {
		return uuid.Nil, false
	}
	if _, err := s.own(ctx, userID, id); err != nil {
		return uuid.Nil, false
	}
	return id, true
}
