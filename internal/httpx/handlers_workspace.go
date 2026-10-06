package httpx

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/chat"
	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	p := Principal(r.Context())
	rows, err := s.DB.ListProjectsForUser(r.Context(), p.UserID)
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 200, rows)
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	p := Principal(r.Context())
	var in struct {
		Name          string          `json:"name"`
		Kind          string          `json:"kind"`
		Settings      json.RawMessage `json:"settings"`
		RepoURL       string          `json:"repo_url"`
		DefaultBranch string          `json:"default_branch"`
	}
	if err := decode(r, &in); err != nil || in.Name == "" {
		writeErr(w, 400, "name required")
		return
	}
	if in.Kind == "" {
		in.Kind = "chat"
	}
	if len(in.Settings) == 0 {
		in.Settings = json.RawMessage("{}")
	}
	in.RepoURL = strings.TrimSpace(in.RepoURL)
	if in.RepoURL != "" && !strings.HasPrefix(in.RepoURL, "https://") {
		writeErr(w, 400, "repo_url must be an https:// clone URL")
		return
	}
	row, err := s.DB.CreateProject(r.Context(), store.CreateProjectParams{OwnerID: p.UserID, Name: in.Name, Kind: in.Kind, Settings: in.Settings})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if in.RepoURL != "" {
		var branch *string
		if b := strings.TrimSpace(in.DefaultBranch); b != "" {
			branch = &b
		}
		if err := s.DB.SetProjectRepo(r.Context(), store.SetProjectRepoParams{ID: row.ID, RepoUrl: &in.RepoURL, DefaultBranch: branch}); err == nil {
			row.RepoUrl = &in.RepoURL
			row.DefaultBranch = branch
		}
		if s.GitHub != nil {
			if id := s.GitHub.ResolveProject(r.Context(), row); id > 0 {
				row.GithubInstallationID = &id
			}
		}
	}
	writeJSON(w, 201, row)
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	id, err := uuidParam(r, "id")
	if err != nil || !s.canAccessProject(r.Context(), id) {
		writeErr(w, 404, "not found")
		return
	}
	row, err := s.DB.GetProject(r.Context(), id)
	if err != nil {
		writeErr(w, 404, "not found")
		return
	}
	writeJSON(w, 200, row)
}

func (s *Server) handleListConversations(w http.ResponseWriter, r *http.Request) {
	id, err := uuidParam(r, "id")
	if err != nil || !s.canAccessProject(r.Context(), id) {
		writeErr(w, 404, "not found")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	rows, err := s.DB.ListConversationsForProject(r.Context(), store.ListConversationsForProjectParams{ProjectID: id, Limit: int32(limit), Offset: int32(offset)})
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 200, rows)
}

func (s *Server) handleRecentConversations(w http.ResponseWriter, r *http.Request) {
	p := Principal(r.Context())
	rows, err := s.DB.ListRecentConversationsForUser(r.Context(), store.ListRecentConversationsForUserParams{UserID: p.UserID, Limit: 50})
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 200, rows)
}

func (s *Server) handleCreateConversation(w http.ResponseWriter, r *http.Request) {
	p := Principal(r.Context())
	id, err := uuidParam(r, "id")
	if err != nil || !s.canAccessProject(r.Context(), id) {
		writeErr(w, 404, "not found")
		return
	}
	var in struct {
		Title    string `json:"title"`
		Mode     string `json:"mode"`
		Selector string `json:"model"`
	}
	_ = decode(r, &in)
	if in.Mode == "" {
		proj, err := s.DB.GetProject(r.Context(), id)
		if err == nil && proj.Kind != "images" {
			in.Mode = proj.Kind
		} else {
			in.Mode = "chat"
		}
	}
	if in.Selector == "" {
		in.Selector = "auto"
	}
	row, err := s.DB.CreateConversation(r.Context(), store.CreateConversationParams{
		ProjectID: id, UserID: p.UserID, Title: in.Title, Mode: in.Mode, ModelSelector: in.Selector, Settings: json.RawMessage("{}"),
	})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 201, row)
}

func (s *Server) handleGetConversation(w http.ResponseWriter, r *http.Request) {
	conv, ok := s.loadConversation(w, r)
	if !ok {
		return
	}
	rows, err := s.DB.ListMessages(r.Context(), conv.ID)
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	// pending approvals on the latest run mark their tool parts
	pending := map[string]string{}
	var runStatus string
	if run, err := s.DB.LatestRunForConversation(r.Context(), conv.ID); err == nil {
		runStatus = run.Status
		if run.Status == "paused_approval" {
			if aps, err := s.DB.ListPendingApprovals(r.Context(), run.ID); err == nil {
				for _, a := range aps {
					pending[a.ToolCallID] = a.ID.String()
				}
			}
		}
	}
	msgs := chat.MergeUI(rows, pending)
	if msgs == nil {
		msgs = []chat.UIMessage{}
	}
	writeJSON(w, 200, map[string]any{"conversation": conv, "messages": msgs, "run_status": runStatus})
}

func (s *Server) handleUpdateConversation(w http.ResponseWriter, r *http.Request) {
	conv, ok := s.loadConversation(w, r)
	if !ok {
		return
	}
	var in struct {
		Title    *string          `json:"title"`
		Selector *string          `json:"model"`
		Settings *json.RawMessage `json:"settings"` // e.g. {"tool_policies":{"web_fetch":"ask"}}
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	if in.Title != nil {
		_ = s.DB.UpdateConversationTitle(r.Context(), store.UpdateConversationTitleParams{ID: conv.ID, Title: *in.Title})
	}
	if in.Selector != nil {
		_ = s.DB.UpdateConversationSelector(r.Context(), store.UpdateConversationSelectorParams{ID: conv.ID, ModelSelector: *in.Selector})
	}
	if in.Settings != nil {
		if !json.Valid(*in.Settings) {
			writeErr(w, 400, "settings must be JSON")
			return
		}
		_ = s.DB.UpdateConversationSettings(r.Context(), store.UpdateConversationSettingsParams{ID: conv.ID, Settings: *in.Settings})
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) handleArchiveConversation(w http.ResponseWriter, r *http.Request) {
	conv, ok := s.loadConversation(w, r)
	if !ok {
		return
	}
	_ = s.DB.ArchiveConversation(r.Context(), conv.ID)
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// handleModels lists routable endpoints plus policy aliases for the picker.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	type model struct {
		ID          string               `json:"id"`
		DisplayName string               `json:"display_name"`
		Provider    string               `json:"provider"`
		Local       bool                 `json:"local"`
		Caps        gateway.Capabilities `json:"capabilities"`
		Pricing     gateway.Pricing      `json:"pricing"`
		Health      string               `json:"health"`
	}
	var out []model
	for _, e := range s.GW.Registry.Endpoints() {
		if !e.Enabled || e.Capabilities.Embeddings || e.Capabilities.IsMedia() {
			continue
		}
		out = append(out, model{ID: e.ID, DisplayName: e.DisplayName, Provider: e.ProviderID, Local: e.Local, Caps: e.Capabilities, Pricing: e.Pricing, Health: e.Health.Status})
	}
	aliases := map[string][]string{"auto": nil}
	for _, p := range s.GW.Router.Policies() {
		for k, v := range p.Aliases {
			if _, exists := aliases[k]; !exists {
				aliases[k] = v
			}
		}
	}
	writeJSON(w, 200, map[string]any{"models": out, "aliases": aliases, "media": s.mediaModels()})
}

// apiKeyView is an API key as the UI sees it: never the hash.
type apiKeyView struct {
	ID            uuid.UUID  `json:"id"`
	Name          string     `json:"name"`
	Prefix        string     `json:"prefix"`
	Scopes        []string   `json:"scopes"`
	DefaultPolicy string     `json:"default_policy"`
	CreatedAt     time.Time  `json:"created_at"`
	LastUsedAt    *time.Time `json:"last_used_at"`
}

func keyView(k store.ApiKey) apiKeyView {
	return apiKeyView{ID: k.ID, Name: k.Name, Prefix: k.Prefix, Scopes: k.Scopes, DefaultPolicy: k.DefaultPolicy, CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt}
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	p := Principal(r.Context())
	rows, err := s.DB.ListAPIKeysByUser(r.Context(), p.UserID)
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	out := make([]apiKeyView, 0, len(rows))
	for _, k := range rows {
		out = append(out, keyView(k))
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	p := Principal(r.Context())
	var in struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
		Policy string   `json:"default_policy"`
	}
	if err := decode(r, &in); err != nil || in.Name == "" {
		writeErr(w, 400, "name required")
		return
	}
	raw, k, err := s.Auth.CreateAPIKey(r.Context(), p.UserID, in.Name, in.Scopes, in.Policy)
	if err != nil {
		if strings.Contains(err.Error(), "unknown scope") {
			writeErr(w, 400, err.Error())
			return
		}
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 201, map[string]any{"key": raw, "record": keyView(*k)})
}

func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	p := Principal(r.Context())
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	_ = s.DB.RevokeAPIKey(r.Context(), store.RevokeAPIKeyParams{ID: id, UserID: p.UserID})
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

var _ = uuid.Nil
