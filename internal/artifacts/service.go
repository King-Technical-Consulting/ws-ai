// Package artifacts stores versioned model-generated documents (HTML, SVG,
// Markdown, Mermaid, code) and serves them on a separate origin inside a
// sandboxed iframe. The app origin never renders artifact content inline.
package artifacts

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

// Kind is the artifact type.
type Kind string

// Kinds the origin can render.
const (
	KindHTML     Kind = "html"
	KindSVG      Kind = "svg"
	KindMarkdown Kind = "markdown"
	KindMermaid  Kind = "mermaid"
	KindCode     Kind = "code"
	KindReact    Kind = "react"  // M6: needs the Sandpack bundler
	KindDesign   Kind = "design" // M6: html with design_context
)

// ValidKind reports whether k is renderable now.
func ValidKind(k Kind) bool {
	switch k {
	case KindHTML, KindSVG, KindMarkdown, KindMermaid, KindCode, KindDesign:
		return true
	}
	return false
}

// MaxContent caps a single version.
const MaxContent = 2 << 20

// Service persists artifacts and signs viewer URLs.
type Service struct {
	DB      *store.DB
	Secret  []byte // HMAC key; the session secret is fine
	BaseURL string // artifact origin, e.g. https://art.example.com
	TTL     time.Duration
}

// Ref is what tools return and the UI consumes.
type Ref struct {
	ArtifactID string `json:"artifact_id"`
	Version    int    `json:"version"`
	VersionID  string `json:"version_id"`
	Kind       Kind   `json:"kind"`
	Title      string `json:"title"`
	URL        string `json:"url"`
}

// Create stores a new artifact with version 1. design is the
// design_context stored on the version (design artifacts; nil otherwise).
func (s *Service) Create(ctx context.Context, convID uuid.UUID, kind Kind, title, language, content string, design json.RawMessage, byMessage uuid.NullUUID) (*Ref, error) {
	if !ValidKind(kind) {
		return nil, fmt.Errorf("artifacts: unsupported kind %q", kind)
	}
	if len(content) > MaxContent {
		return nil, errors.New("artifacts: content too large")
	}
	design, err := normalizeDesign(kind, design)
	if err != nil {
		return nil, err
	}
	if title == "" {
		title = "Untitled"
	}
	var lang *string
	if language != "" {
		lang = &language
	}
	a, err := s.DB.CreateArtifact(ctx, store.CreateArtifactParams{ConversationID: convID, Kind: string(kind), Title: title, Language: lang})
	if err != nil {
		return nil, err
	}
	return s.addVersion(ctx, a.ID, kind, title, content, design, byMessage)
}

// Update appends a version. An empty title keeps the old one; a nil
// design keeps the previous version's design_context.
func (s *Service) Update(ctx context.Context, artifactID uuid.UUID, title, content string, design json.RawMessage, byMessage uuid.NullUUID) (*Ref, error) {
	if len(content) > MaxContent {
		return nil, errors.New("artifacts: content too large")
	}
	a, err := s.DB.GetArtifact(ctx, artifactID)
	if err != nil {
		return nil, fmt.Errorf("artifacts: not found")
	}
	design, err = normalizeDesign(Kind(a.Kind), design)
	if err != nil {
		return nil, err
	}
	if design == nil && Kind(a.Kind) == KindDesign {
		if prev, err := s.DB.GetArtifactVersion(ctx, store.GetArtifactVersionParams{ArtifactID: a.ID, Version: a.CurrentVersion}); err == nil {
			design = prev.DesignContext
		}
	}
	if title == "" {
		title = a.Title
	}
	return s.addVersion(ctx, a.ID, Kind(a.Kind), title, content, design, byMessage)
}

// normalizeDesign validates a design_context and drops it for kinds that
// do not carry one.
func normalizeDesign(kind Kind, raw json.RawMessage) (json.RawMessage, error) {
	if kind != KindDesign {
		return nil, nil
	}
	d, err := ParseDesignContext(raw)
	if err != nil {
		return nil, fmt.Errorf("artifacts: %w", err)
	}
	if d == nil {
		return nil, nil
	}
	b, _ := json.Marshal(d)
	return b, nil
}

func (s *Service) addVersion(ctx context.Context, id uuid.UUID, kind Kind, title, content string, design json.RawMessage, byMessage uuid.NullUUID) (*Ref, error) {
	v, err := s.DB.BumpArtifactVersion(ctx, store.BumpArtifactVersionParams{ID: id, Column2: title})
	if err != nil {
		return nil, err
	}
	row, err := s.DB.InsertArtifactVersion(ctx, store.InsertArtifactVersionParams{
		ArtifactID: id, Version: v, Content: &content, DesignContext: design, CreatedByMessageID: byMessage,
	})
	if err != nil {
		return nil, err
	}
	return &Ref{ArtifactID: id.String(), Version: int(v), VersionID: row.ID.String(), Kind: kind, Title: title, URL: s.SignedURL(row.ID)}, nil
}

// Get returns the artifact, its version list, and the requested version
// (0 = current) with content and a signed URL.
func (s *Service) Get(ctx context.Context, id uuid.UUID, version int) (*store.Artifact, []store.ListArtifactVersionsRow, *store.ArtifactVersion, *Ref, error) {
	a, err := s.DB.GetArtifact(ctx, id)
	if err != nil {
		return nil, nil, nil, nil, errors.New("artifacts: not found")
	}
	versions, err := s.DB.ListArtifactVersions(ctx, id)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if version <= 0 {
		version = int(a.CurrentVersion)
	}
	v, err := s.DB.GetArtifactVersion(ctx, store.GetArtifactVersionParams{ArtifactID: id, Version: int32(version)})
	if err != nil {
		return nil, nil, nil, nil, errors.New("artifacts: version not found")
	}
	ref := &Ref{ArtifactID: id.String(), Version: version, VersionID: v.ID.String(), Kind: Kind(a.Kind), Title: a.Title, URL: s.SignedURL(v.ID)}
	return &a, versions, &v, ref, nil
}

// ---- signed URLs ----

// SignedURL returns BaseURL/a/{versionID}?t={exp}.{sig}.
func (s *Service) SignedURL(versionID uuid.UUID) string {
	ttl := s.TTL
	if ttl == 0 {
		ttl = 24 * time.Hour
	}
	exp := time.Now().Add(ttl).Unix()
	return fmt.Sprintf("%s/a/%s?t=%d.%s", strings.TrimRight(s.BaseURL, "/"), versionID, exp, s.sign(versionID, exp))
}

func (s *Service) sign(id uuid.UUID, exp int64) string {
	m := hmac.New(sha256.New, s.Secret)
	m.Write([]byte(id.String()))
	m.Write([]byte{'|'})
	m.Write([]byte(strconv.FormatInt(exp, 10)))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Verify checks a token for a version id.
func (s *Service) Verify(id uuid.UUID, token string) bool {
	dot := strings.IndexByte(token, '.')
	if dot < 0 {
		return false
	}
	exp, err := strconv.ParseInt(token[:dot], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(s.sign(id, exp)), []byte(token[dot+1:]))
}

// ---- tools the model can call ----

// ToolDefs returns the artifact tools for chat.
func ToolDefs() []gateway.ToolDef {
	return []gateway.ToolDef{
		{
			Name:        "create_artifact",
			Description: "Create a substantial, self-contained document the user can view in a side panel and keep: a complete HTML page (with inline CSS/JS), a UI design (kind design: a complete HTML mockup that follows a design system), an SVG, a Mermaid diagram, a Markdown document, or a code file. Use it for content over ~15 lines that the user will reuse, edit, or run. Do not use it for short answers or explanations; put those in the reply. After calling it, briefly tell the user what you made; do not repeat the content.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "kind": {"type": "string", "enum": ["html", "design", "svg", "markdown", "mermaid", "code"], "description": "What the content is. design is an HTML mockup of a user interface."},
    "title": {"type": "string", "description": "Short title, 2-6 words."},
    "language": {"type": "string", "description": "For kind=code: the language, e.g. python, go, tsx."},
    "content": {"type": "string", "description": "The full content. For html and design, a complete document."},
    "design_context": {
      "type": "object",
      "description": "For kind=design: the design system the mockup follows, so later versions and variants keep it. Copy the conversation's design system when there is one.",
      "properties": {
        "library": {"type": "string", "enum": ["tailwind", "shadcn", "plain"]},
        "colors": {"type": "object", "additionalProperties": {"type": "string"}, "description": "Token name to CSS color, e.g. primary, background, foreground, accent."},
        "type": {"type": "object", "additionalProperties": {"type": "string"}, "description": "heading, body, mono font stacks; scale."},
        "spacing": {"type": "string"},
        "radius": {"type": "string"},
        "components": {"type": "array", "items": {"type": "string"}},
        "notes": {"type": "string"}
      },
      "additionalProperties": false
    }
  },
  "required": ["kind", "title", "content"],
  "additionalProperties": false
}`),
		},
		{
			Name:        "update_artifact",
			Description: "Replace the content of an existing artifact with a new version. Always send the complete new content, not a diff. A design artifact keeps its design_context unless you send a new one.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "artifact_id": {"type": "string", "description": "The artifact_id returned by create_artifact."},
    "title": {"type": "string", "description": "New title, or omit to keep it."},
    "content": {"type": "string", "description": "The complete new content."},
    "design_context": {"type": "object", "description": "For a design artifact: a changed design system; omit to keep the current one.", "additionalProperties": true}
  },
  "required": ["artifact_id", "content"],
  "additionalProperties": false
}`),
		},
	}
}

// IsTool reports whether a tool name belongs to this package.
func IsTool(name string) bool { return name == "create_artifact" || name == "update_artifact" }

// Call executes an artifact tool and returns the Ref as JSON for the model.
func (s *Service) Call(ctx context.Context, convID uuid.UUID, byMessage uuid.NullUUID, name string, args json.RawMessage) (*Ref, error) {
	switch name {
	case "create_artifact":
		var in struct {
			Kind     Kind            `json:"kind"`
			Title    string          `json:"title"`
			Language string          `json:"language"`
			Content  string          `json:"content"`
			Design   json.RawMessage `json:"design_context"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("bad arguments: %w", err)
		}
		return s.Create(ctx, convID, in.Kind, in.Title, in.Language, in.Content, in.Design, byMessage)
	case "update_artifact":
		var in struct {
			ArtifactID string          `json:"artifact_id"`
			Title      string          `json:"title"`
			Content    string          `json:"content"`
			Design     json.RawMessage `json:"design_context"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("bad arguments: %w", err)
		}
		id, err := uuid.Parse(in.ArtifactID)
		if err != nil {
			return nil, errors.New("artifact_id must be the id returned by create_artifact")
		}
		// The artifact must belong to this conversation.
		a, err := s.DB.GetArtifact(ctx, id)
		if err != nil || a.ConversationID != convID {
			return nil, errors.New("artifact not found in this conversation")
		}
		return s.Update(ctx, id, in.Title, in.Content, in.Design, byMessage)
	}
	return nil, fmt.Errorf("unknown tool %s", name)
}
