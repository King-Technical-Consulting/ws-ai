package httpx

import (
	"net/http"

	"github.com/jking323/ws/internal/github"
)

// handleGitHubStatus tells the admin whether the GitHub App is configured
// and where it is installed.
func (s *Server) handleGitHubStatus(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"configured": false, "fallback_token": s.Cfg.GitHubToken != ""}
	if s.GitHub == nil || !s.GitHub.Configured() {
		writeJSON(w, 200, out)
		return
	}
	out["configured"] = true
	out["slug"] = s.GitHub.App.Slug()
	out["install_url"] = s.GitHub.App.InstallURL()
	insts, err := s.GitHub.App.Installations(r.Context())
	if err != nil {
		out["error"] = err.Error()
	}
	if insts == nil {
		insts = []github.Installation{} // "configured but installed nowhere" is a list, not null
	}
	out["installations"] = insts
	writeJSON(w, 200, out)
}
