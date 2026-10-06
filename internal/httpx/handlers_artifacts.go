package httpx

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jking323/ws/internal/artifacts"
	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

// artifactAllowed checks the caller may see the artifact and returns its
// conversation.
func (s *Server) artifactAllowed(w http.ResponseWriter, r *http.Request) (uuid.UUID, *store.Conversation, bool) {
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return uuid.Nil, nil, false
	}
	conv, err := s.DB.GetArtifactConversation(r.Context(), id)
	if err != nil || !s.canAccessProject(r.Context(), conv.ProjectID) {
		writeErr(w, 404, "not found")
		return uuid.Nil, nil, false
	}
	return id, &conv, true
}

func requestedVersion(r *http.Request) int {
	version, _ := strconv.Atoi(r.URL.Query().Get("version"))
	if v := chi.URLParam(r, "v"); v != "" {
		version, _ = strconv.Atoi(v)
	}
	return version
}

// handleGetArtifact returns the artifact, its versions, and one version
// (the current one, or ?version=N) with content, its design_context and
// a signed viewer URL.
func (s *Server) handleGetArtifact(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.artifactAllowed(w, r)
	if !ok {
		return
	}
	a, versions, v, ref, err := s.Artifacts.Get(r.Context(), id, requestedVersion(r))
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	content := ""
	if v.Content != nil {
		content = *v.Content
	}
	var design json.RawMessage
	if len(v.DesignContext) > 0 {
		design = v.DesignContext
	}
	writeJSON(w, 200, map[string]any{
		"artifact": a, "versions": versions, "version": ref.Version, "version_id": ref.VersionID,
		"url": ref.URL, "content": content, "kind": ref.Kind, "title": a.Title, "design_context": design,
	})
}

func (s *Server) handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	conv, ok := s.loadConversation(w, r)
	if !ok {
		return
	}
	rows, err := s.DB.ListArtifactsForConversation(r.Context(), conv.ID)
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 200, rows)
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// exportName is the download file name for a version: the title as a
// slug, the version, and an extension for the kind.
func exportName(a *store.Artifact, version int) (name, mime string) {
	ext, mime := ".html", "text/html; charset=utf-8"
	switch artifacts.Kind(a.Kind) {
	case artifacts.KindSVG:
		ext, mime = ".svg", "image/svg+xml; charset=utf-8"
	case artifacts.KindMarkdown:
		ext, mime = ".md", "text/markdown; charset=utf-8"
	case artifacts.KindMermaid:
		ext, mime = ".mmd", "text/plain; charset=utf-8"
	case artifacts.KindCode:
		ext, mime = ".txt", "text/plain; charset=utf-8"
		if a.Language != nil {
			if e, ok := codeExt[strings.ToLower(*a.Language)]; ok {
				ext = e
			}
		}
	}
	slug := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(a.Title), "-"), "-")
	if slug == "" {
		slug = "artifact"
	}
	if len(slug) > 60 {
		slug = slug[:60]
	}
	return fmt.Sprintf("%s-v%d%s", slug, version, ext), mime
}

var codeExt = map[string]string{
	"go": ".go", "python": ".py", "py": ".py", "typescript": ".ts", "ts": ".ts", "tsx": ".tsx", "javascript": ".js", "js": ".js", "jsx": ".jsx",
	"rust": ".rs", "java": ".java", "kotlin": ".kt", "swift": ".swift", "c": ".c", "cpp": ".cpp", "c++": ".cpp", "csharp": ".cs", "ruby": ".rb",
	"php": ".php", "shell": ".sh", "bash": ".sh", "sh": ".sh", "sql": ".sql", "yaml": ".yaml", "yml": ".yaml", "json": ".json", "toml": ".toml",
	"html": ".html", "css": ".css", "scss": ".scss", "markdown": ".md", "md": ".md", "dockerfile": ".Dockerfile", "makefile": ".mk",
}

// handleExportArtifact is GET /api/artifacts/{id}/export?version=N: the
// version's content as a download (the model's document, without the
// viewer runtime), never rendered on this origin.
func (s *Server) handleExportArtifact(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.artifactAllowed(w, r)
	if !ok {
		return
	}
	a, _, v, ref, err := s.Artifacts.Get(r.Context(), id, requestedVersion(r))
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	content := ""
	if v.Content != nil {
		content = *v.Content
	}
	name, mime := exportName(a, ref.Version)
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(200)
	_, _ = w.Write([]byte(content))
}

// handleArtifactVariants is POST /api/artifacts/{id}/variants: N
// alternatives of a design (or html) artifact's version, each stored as
// a new design artifact in the conversation. Body {n, instruction,
// version, model}. Each variant is a model call billed to the caller.
func (s *Server) handleArtifactVariants(w http.ResponseWriter, r *http.Request) {
	id, conv, ok := s.artifactAllowed(w, r)
	if !ok {
		return
	}
	if s.GW == nil {
		writeErr(w, 503, "no gateway")
		return
	}
	var in struct {
		N           int    `json:"n"`
		Instruction string `json:"instruction"`
		Version     int    `json:"version"`
		Model       string `json:"model"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	p := Principal(r.Context())
	refs, errs, err := s.Artifacts.Variants(r.Context(), s.GW, artifacts.VariantParams{
		ArtifactID: id, Version: in.Version, N: in.N, Instruction: in.Instruction, Selector: in.Model,
		Meta: gateway.Metadata{UserID: p.UserID.String(), ConversationID: conv.ID.String(), TaskClass: gateway.TaskChat},
	})
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	writeJSON(w, 201, map[string]any{"variants": refs, "errors": msgs})
}
