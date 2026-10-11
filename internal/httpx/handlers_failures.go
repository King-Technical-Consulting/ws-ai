package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// Recent failed background jobs, read from River's own table (river_job), so the owner can
// see in Admin what the worker gave up on without reading its log. Nothing is copied: the
// list is a query, and "clear" only marks the rows it showed with a metadata flag so the
// list skips them (the jobs stay where they are, and River's own retention still removes
// discarded rows after its retention period, a week by default).

const failuresLimit = 50

// maxFailureText caps one job's error text: a failure can carry a long trace.
const maxFailureText = 600

type failureRef struct {
	Label string `json:"label"`
	Link  string `json:"link"`
}

type failureRow struct {
	ID          int64       `json:"id"`
	Kind        string      `json:"kind"`
	State       string      `json:"state"` // discarded: gave up; retryable: failed and waiting to retry
	Attempt     int         `json:"attempt"`
	MaxAttempts int         `json:"max_attempts"`
	At          *time.Time  `json:"at"`
	Error       string      `json:"error"`
	Ref         *failureRef `json:"ref"`
}

// failureRef names the thing a job was about where its arguments say so, with the page that
// shows it. Training and media have no page per item, so they link to the page.
func failureReference(kind string, args map[string]json.RawMessage, agentOf map[uuid.UUID]uuid.UUID) *failureRef {
	str := func(key string) string {
		var s string
		_ = json.Unmarshal(args[key], &s)
		return s
	}
	switch kind {
	case "agent.run", "agent.resume":
		if id, err := uuid.Parse(str("run_id")); err == nil {
			if agent, ok := agentOf[id]; ok {
				return &failureRef{Label: "agent run " + id.String()[:8], Link: "/agents/" + agent.String()}
			}
			return &failureRef{Label: "agent run " + id.String()[:8], Link: "/agents"}
		}
	case "media.generate":
		if id := str("job_id"); id != "" {
			return &failureRef{Label: "media job " + short8(id), Link: "/media"}
		}
	case "training.build":
		if id := str("dataset_id"); id != "" {
			return &failureRef{Label: "dataset " + short8(id), Link: "/training"}
		}
	case "training.finetune":
		if id := str("job_id"); id != "" {
			return &failureRef{Label: "fine-tune " + short8(id), Link: "/training"}
		}
	case "training.eval":
		if id := str("adapter_id"); id != "" {
			return &failureRef{Label: "adapter " + short8(id), Link: "/training"}
		}
	case "conversation.compact":
		if id := str("conversation_id"); id != "" {
			return &failureRef{Label: "conversation " + short8(id), Link: "/c/" + id}
		}
	}
	return nil
}

func short8(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// lastJobError is the text of the newest entry of a job's `errors` array ({at, attempt, error,
// trace}), cut to maxFailureText runes.
func lastJobError(raw []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	r := []rune(e.Error)
	if len(r) > maxFailureText {
		return string(r[:maxFailureText]) + "…"
	}
	return e.Error
}

const failuresWhere = `state IN ('discarded', 'retryable') AND COALESCE(metadata->>'ws_hidden', '') <> 'true'`

// handleFailures lists the newest failed jobs of every kind. Owner only.
func (s *Server) handleFailures(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Pool.Query(r.Context(), `
		SELECT id, kind, state::text, attempt, max_attempts, COALESCE(finalized_at, attempted_at), args,
		       (errors)[COALESCE(array_length(errors, 1), 0)]
		FROM river_job
		WHERE `+failuresWhere+`
		ORDER BY COALESCE(finalized_at, attempted_at) DESC NULLS LAST, id DESC
		LIMIT $1`, failuresLimit)
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	defer rows.Close()
	out := []failureRow{}
	var runIDs []uuid.UUID
	argsOf := map[int64]map[string]json.RawMessage{}
	for rows.Next() {
		var (
			f    failureRow
			args []byte
			last []byte
			att  int16
			maxA int16
		)
		if err := rows.Scan(&f.ID, &f.Kind, &f.State, &att, &maxA, &f.At, &args, &last); err != nil {
			writeErr(w, 500, "db")
			return
		}
		f.Attempt, f.MaxAttempts = int(att), int(maxA)
		f.Error = lastJobError(last)
		var a map[string]json.RawMessage
		_ = json.Unmarshal(args, &a)
		argsOf[f.ID] = a
		if f.Kind == "agent.run" || f.Kind == "agent.resume" {
			var id string
			_ = json.Unmarshal(a["run_id"], &id)
			if u, err := uuid.Parse(id); err == nil {
				runIDs = append(runIDs, u)
			}
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, 500, "db")
		return
	}
	agentOf := s.agentsOfRuns(r.Context(), runIDs)
	for i := range out {
		out[i].Ref = failureReference(out[i].Kind, argsOf[out[i].ID], agentOf)
	}
	writeJSON(w, 200, out)
}

// agentsOfRuns maps run ids to their agent where the run still exists and belongs to one.
func (s *Server) agentsOfRuns(ctx context.Context, ids []uuid.UUID) map[uuid.UUID]uuid.UUID {
	m := map[uuid.UUID]uuid.UUID{}
	if len(ids) == 0 {
		return m
	}
	rows, err := s.DB.Pool.Query(ctx, `SELECT id, agent_id FROM agent_runs WHERE id = ANY($1) AND agent_id IS NOT NULL`, ids)
	if err != nil {
		return m
	}
	defer rows.Close()
	for rows.Next() {
		var id, agent uuid.UUID
		if rows.Scan(&id, &agent) == nil {
			m[id] = agent
		}
	}
	return m
}

// handleClearFailures hides the failures now listed (and any older still failing) from the
// list. It deletes nothing and touches no job state; a later failure shows up as usual.
func (s *Server) handleClearFailures(w http.ResponseWriter, r *http.Request) {
	tag, err := s.DB.Pool.Exec(r.Context(), `
		UPDATE river_job SET metadata = metadata || '{"ws_hidden": true}'::jsonb
		WHERE `+failuresWhere)
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 200, map[string]int64{"hidden": tag.RowsAffected()})
}
