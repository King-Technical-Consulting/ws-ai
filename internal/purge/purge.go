// Package purge is permanent deletion of archived conversations and
// projects. Archiving only hides; this removes: the rows (messages,
// attachments, artifacts and their versions, agent runs and steps, document
// chunks, memory provenance links) and the files on disk that nothing else
// still uses. The usage ledger keeps its rows for spend history with the
// conversation and run references cleared.
//
// What is kept, on purpose:
//   - an attachment that another conversation, a media job that survives, or
//     a notebook source still points at (its row and its blob);
//   - a blob whose bytes another attachment, an artifact version, a dataset
//     or an adapter also uses (blobs are content-addressed and shared);
//   - media jobs and their outputs when only a conversation is deleted: the
//     gallery belongs to the project, not to the chat that asked for them;
//   - memories the member kept: only their link to the conversation goes;
//   - notebook sources made from a conversation: their text is a copy, so
//     they stay as text with the link cleared.
package purge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jking323/ws/internal/store/blob"
)

// ErrBusy is returned when an agent run in the set is still open.
var ErrBusy = errors.New("purge: an agent run in it is still open")

// ErrNotArchived is returned for a conversation or project that is not
// archived: only archived items are deleted.
var ErrNotArchived = errors.New("purge: not archived")

// Sandboxes removes a project's sandbox containers and volumes. The worker
// holds the Docker socket, so serve reaches it through the worker client.
type Sandboxes interface {
	RemoveProject(ctx context.Context, projectID uuid.UUID) error
}

// Purger deletes archived conversations and projects.
type Purger struct {
	Pool      *pgxpool.Pool
	Blobs     blob.Store
	Sandboxes Sandboxes // nil when sandboxes are off
	Log       *slog.Logger
}

// Plan is what a deletion removes, counted before it happens.
type Plan struct {
	Conversations int `json:"conversations"`
	Messages      int `json:"messages"`
	Artifacts     int `json:"artifacts"`
	Runs          int `json:"runs"`
	// Attachments counts files that go with it: not used by anything that
	// stays.
	Attachments int `json:"attachments"`
	// Agents and Sandboxes are counted for a project only.
	Agents    int `json:"agents,omitempty"`
	Sandboxes int `json:"sandboxes,omitempty"`
	// Bytes is the space it frees: message text, artifact content and the
	// attachments that go with it.
	Bytes int64 `json:"bytes"`
}

// Report is what a deletion did.
type Report struct {
	Plan
	BlobsDeleted int `json:"blobs_deleted"`
	// BlobsShared were kept because another row still uses the same bytes.
	BlobsShared int `json:"blobs_shared"`
	// BlobsFailed could not be removed from disk; the rows are gone.
	BlobsFailed int `json:"blobs_failed"`
}

func (p *Purger) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

// attachmentCandidates is the SQL for the attachments a deletion could free:
// those on the messages of the conversations ($1) and, for a project ($2),
// the outputs of its media jobs. A candidate goes only when nothing that
// stays uses it (attachmentFree).
const attachmentCandidates = `
SELECT DISTINCT ma.attachment_id AS id
  FROM message_attachments ma JOIN messages m ON m.id = ma.message_id
 WHERE m.conversation_id = ANY($1)
UNION
SELECT unnest(j.output_attachment_ids) FROM media_jobs j WHERE j.project_id = $2
UNION
SELECT (j.inputs->>'source_attachment_id')::uuid FROM media_jobs j
 WHERE j.project_id = $2 AND j.inputs->>'source_attachment_id' ~ '^[0-9a-fA-F-]{36}$'`

// attachmentFree is the guard: the attachment is not used by a message of a
// conversation that stays, a media job that stays, or a notebook source.
const attachmentFree = `
NOT EXISTS (SELECT 1 FROM message_attachments ma JOIN messages m ON m.id = ma.message_id
             WHERE ma.attachment_id = a.id AND NOT (m.conversation_id = ANY($1)))
AND NOT EXISTS (SELECT 1 FROM media_jobs j
                 WHERE j.project_id IS DISTINCT FROM $2
                   AND (a.id = ANY(j.output_attachment_ids) OR j.inputs->>'source_attachment_id' = a.id::text))
AND NOT EXISTS (SELECT 1 FROM notebook_sources s WHERE s.attachment_id = a.id)`

func nullUUID(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return *id
}

// plan counts what deleting convs (and project, when set) removes.
func plan(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, convs []uuid.UUID, project *uuid.UUID) (Plan, error) {
	pl := Plan{Conversations: len(convs)}
	var textBytes, artBytes int64
	if err := q.QueryRow(ctx, `SELECT count(*), COALESCE(sum(octet_length(parts::text)), 0)::bigint FROM messages WHERE conversation_id = ANY($1)`, convs).Scan(&pl.Messages, &textBytes); err != nil {
		return pl, err
	}
	if err := q.QueryRow(ctx, `SELECT count(DISTINCT a.id), COALESCE(sum(octet_length(v.content)), 0)::bigint
	    FROM artifacts a LEFT JOIN artifact_versions v ON v.artifact_id = a.id WHERE a.conversation_id = ANY($1)`, convs).Scan(&pl.Artifacts, &artBytes); err != nil {
		return pl, err
	}
	if err := q.QueryRow(ctx, `SELECT count(*) FROM agent_runs WHERE conversation_id = ANY($1)`, convs).Scan(&pl.Runs); err != nil {
		return pl, err
	}
	var attBytes int64
	if err := q.QueryRow(ctx, `SELECT count(*), COALESCE(sum(a.bytes), 0)::bigint FROM attachments a
	    WHERE a.id IN (`+attachmentCandidates+`) AND `+attachmentFree, convs, nullUUID(project)).Scan(&pl.Attachments, &attBytes); err != nil {
		return pl, err
	}
	pl.Bytes = textBytes + artBytes + attBytes
	if project != nil {
		if err := q.QueryRow(ctx, `SELECT count(*) FROM agents WHERE project_id = $1`, *project).Scan(&pl.Agents); err != nil {
			return pl, err
		}
		if err := q.QueryRow(ctx, `SELECT count(*) FROM sandboxes WHERE project_id = $1`, *project).Scan(&pl.Sandboxes); err != nil {
			return pl, err
		}
	}
	return pl, nil
}

// PlanConversations counts what deleting these conversations removes.
func (p *Purger) PlanConversations(ctx context.Context, convs []uuid.UUID) (Plan, error) {
	return plan(ctx, p.Pool, convs, nil)
}

// PlanProject counts what deleting a project removes, for every
// conversation in it, archived or not.
func (p *Purger) PlanProject(ctx context.Context, projectID uuid.UUID) (Plan, error) {
	convs, err := p.projectConversations(ctx, p.Pool, projectID)
	if err != nil {
		return Plan{}, err
	}
	return plan(ctx, p.Pool, convs, &projectID)
}

func (p *Purger) projectConversations(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, projectID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, `SELECT id FROM conversations WHERE project_id = $1`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if ids == nil {
		ids = []uuid.UUID{}
	}
	return ids, rows.Err()
}

// DeleteConversations permanently deletes archived conversations. A
// conversation that is not archived makes the whole call fail with
// ErrNotArchived and nothing is deleted.
func (p *Purger) DeleteConversations(ctx context.Context, convs []uuid.UUID) (Report, error) {
	if len(convs) == 0 {
		return Report{}, nil
	}
	return p.run(ctx, convs, nil)
}

// DeleteProject permanently deletes an archived project with every
// conversation, agent, sandbox and file in it.
func (p *Purger) DeleteProject(ctx context.Context, projectID uuid.UUID) (Report, error) {
	var archived bool
	if err := p.Pool.QueryRow(ctx, `SELECT archived_at IS NOT NULL FROM projects WHERE id = $1`, projectID).Scan(&archived); err != nil {
		return Report{}, err
	}
	if !archived {
		return Report{}, ErrNotArchived
	}
	convs, err := p.projectConversations(ctx, p.Pool, projectID)
	if err != nil {
		return Report{}, err
	}
	if p.Sandboxes != nil {
		// Idempotent; before the rows, so a failure leaves the project in
		// place and the person can try again.
		if err := p.Sandboxes.RemoveProject(ctx, projectID); err != nil {
			return Report{}, fmt.Errorf("purge: remove sandboxes: %w", err)
		}
	} else {
		var n int
		if err := p.Pool.QueryRow(ctx, `SELECT count(*) FROM sandboxes WHERE project_id = $1`, projectID).Scan(&n); err != nil {
			return Report{}, err
		}
		if n > 0 {
			return Report{}, errors.New("purge: the project has a sandbox and no worker is connected to remove it")
		}
	}
	return p.run(ctx, convs, &projectID)
}

func (p *Purger) run(ctx context.Context, convs []uuid.UUID, project *uuid.UUID) (Report, error) {
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return Report{}, err
	}
	defer tx.Rollback(ctx)

	if project == nil {
		var notArchived int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM conversations WHERE id = ANY($1) AND archived_at IS NULL`, convs).Scan(&notArchived); err != nil {
			return Report{}, err
		}
		if notArchived > 0 {
			return Report{}, ErrNotArchived
		}
	}
	var open int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM agent_runs
	    WHERE conversation_id = ANY($1) AND status IN ('queued','running','paused_approval','paused_steer','paused_manual')`, convs).Scan(&open); err != nil {
		return Report{}, err
	}
	if open > 0 {
		return Report{}, ErrBusy
	}

	var rep Report
	if rep.Plan, err = plan(ctx, tx, convs, project); err != nil {
		return rep, err
	}
	// Artifact blobs: read before the rows go.
	keys := map[string]bool{}
	rows, err := tx.Query(ctx, `SELECT v.blob_key FROM artifact_versions v JOIN artifacts a ON a.id = v.artifact_id
	    WHERE a.conversation_id = ANY($1) AND v.blob_key IS NOT NULL`, convs)
	if err != nil {
		return rep, err
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return rep, err
		}
		keys[k] = true
	}
	rows.Close()
	var cand []uuid.UUID
	crows, err := tx.Query(ctx, `SELECT id FROM (`+attachmentCandidates+`) c WHERE id IS NOT NULL`, convs, nullUUID(project))
	if err != nil {
		return rep, err
	}
	for crows.Next() {
		var id uuid.UUID
		if err := crows.Scan(&id); err != nil {
			crows.Close()
			return rep, err
		}
		cand = append(cand, id)
	}
	crows.Close()

	// The ledger keeps its rows; the references to what goes are cleared.
	if _, err := tx.Exec(ctx, `UPDATE usage_ledger SET agent_run_id = NULL
	    WHERE agent_run_id IN (SELECT id FROM agent_runs WHERE conversation_id = ANY($1))`, convs); err != nil {
		return rep, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM favorites WHERE conversation_id = ANY($1)`, convs); err != nil {
		return rep, err
	}
	if project != nil {
		// Cascades: conversations and what hangs on them, agents and their
		// triggers and runs, sandbox rows, media jobs, scheduled prompts.
		if _, err := tx.Exec(ctx, `DELETE FROM projects WHERE id = $1`, *project); err != nil {
			return rep, err
		}
	} else if _, err := tx.Exec(ctx, `DELETE FROM conversations WHERE id = ANY($1)`, convs); err != nil {
		return rep, err
	}
	// Now the attachments nothing that stays uses (documents and their
	// chunks cascade from the row).
	del, err := tx.Query(ctx, `DELETE FROM attachments a WHERE a.id = ANY($1) AND `+attachmentFreeAfter+` RETURNING a.blob_key`, cand)
	if err != nil {
		return rep, err
	}
	for del.Next() {
		var k string
		if err := del.Scan(&k); err != nil {
			del.Close()
			return rep, err
		}
		keys[k] = true
	}
	del.Close()
	if err := del.Err(); err != nil {
		return rep, err
	}
	if err := tx.Commit(ctx); err != nil {
		return rep, err
	}

	// Files last, once the rows are gone: a key that any row still uses stays.
	for k := range keys {
		var used bool
		if err := p.Pool.QueryRow(ctx, blobInUse, k).Scan(&used); err != nil {
			rep.BlobsFailed++
			continue
		}
		if used {
			rep.BlobsShared++
			continue
		}
		if err := p.Blobs.Delete(ctx, k); err != nil {
			p.log().Warn("purge: blob not removed", "key", k, "err", err)
			rep.BlobsFailed++
			continue
		}
		if _, err := p.Blobs.Stat(ctx, k); !errors.Is(err, blob.ErrNotFound) {
			p.log().Warn("purge: blob still present after delete", "key", k, "err", err)
			rep.BlobsFailed++
			continue
		}
		rep.BlobsDeleted++
	}
	p.log().Info("purge: deleted", "conversations", rep.Conversations, "project", project != nil, "attachments", rep.Attachments,
		"blobs_deleted", rep.BlobsDeleted, "blobs_shared", rep.BlobsShared, "blobs_failed", rep.BlobsFailed, "bytes", rep.Bytes)
	return rep, nil
}

// attachmentFreeAfter is the guard once the conversations (and, for a
// project, its media jobs) are already gone inside the transaction: whatever
// message, job or notebook source still points at the attachment stays.
const attachmentFreeAfter = `
NOT EXISTS (SELECT 1 FROM message_attachments ma WHERE ma.attachment_id = a.id)
AND NOT EXISTS (SELECT 1 FROM media_jobs j
                 WHERE a.id = ANY(j.output_attachment_ids) OR j.inputs->>'source_attachment_id' = a.id::text)
AND NOT EXISTS (SELECT 1 FROM notebook_sources s WHERE s.attachment_id = a.id)`

// blobInUse reports whether any row still refers to a blob key. A member's
// approved skill files (the shelf, internal/agents/skillstore.go) live in the
// blob store by hash too, so their inventories count.
const blobInUse = `
SELECT EXISTS (SELECT 1 FROM attachments WHERE blob_key = $1)
    OR EXISTS (SELECT 1 FROM artifact_versions WHERE blob_key = $1)
    OR EXISTS (SELECT 1 FROM datasets WHERE blob_key = $1 OR eval_blob_key = $1 OR source_blob_key = $1)
    OR EXISTS (SELECT 1 FROM adapters WHERE blob_key = $1)
    OR EXISTS (SELECT 1 FROM skills s, jsonb_array_elements(s.files) f
                WHERE f->>'sha256' = substr($1, 8) AND COALESCE((f->>'refused')::boolean, false) = false)`

// ArchivedConversation is one row of the Archived view.
type ArchivedConversation struct {
	ID          uuid.UUID `json:"id"`
	ProjectID   uuid.UUID `json:"project_id"`
	ProjectName string    `json:"project_name"`
	Title       string    `json:"title"`
	Mode        string    `json:"mode"`
	ArchivedAt  time.Time `json:"archived_at"`
	Messages    int       `json:"messages"`
	Bytes       int64     `json:"bytes"`
	// CanDelete is true when the caller may delete it permanently: the
	// conversation's own member, or the project's owner.
	CanDelete bool `json:"can_delete"`
}

// ArchivedProject is one archived project the caller owns.
type ArchivedProject struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	Kind       string    `json:"kind"`
	ArchivedAt time.Time `json:"archived_at"`
	Plan
}

// Archived is what the Archived view shows.
type Archived struct {
	Conversations []ArchivedConversation `json:"conversations"`
	Projects      []ArchivedProject      `json:"projects"`
	// All is what "Delete all archived" would remove: every archived
	// conversation listed, counted together (a file two of them share is
	// counted once).
	All Plan `json:"all"`
}

// maxArchivedList bounds the view; a longer list is cut and says so by its
// All count being larger than the rows shown.
const maxArchivedList = 500

// ListArchived lists the archived conversations the caller may delete (their
// own, and any in a project they own) whose project is not itself archived,
// and the archived projects they own, each with what deleting it frees.
func (p *Purger) ListArchived(ctx context.Context, userID uuid.UUID) (*Archived, error) {
	out := &Archived{Conversations: []ArchivedConversation{}, Projects: []ArchivedProject{}}
	rows, err := p.Pool.Query(ctx, `
	    SELECT c.id, c.project_id, pr.name, c.title, c.mode, c.archived_at, (c.user_id = $1 OR pr.owner_id = $1)
	      FROM conversations c JOIN projects pr ON pr.id = c.project_id
	     WHERE c.archived_at IS NOT NULL AND pr.archived_at IS NULL AND (c.user_id = $1 OR pr.owner_id = $1)
	     ORDER BY c.archived_at DESC LIMIT $2`, userID, maxArchivedList)
	if err != nil {
		return nil, err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var c ArchivedConversation
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.ProjectName, &c.Title, &c.Mode, &c.ArchivedAt, &c.CanDelete); err != nil {
			rows.Close()
			return nil, err
		}
		out.Conversations = append(out.Conversations, c)
		ids = append(ids, c.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out.Conversations {
		pl, err := plan(ctx, p.Pool, []uuid.UUID{out.Conversations[i].ID}, nil)
		if err != nil {
			return nil, err
		}
		out.Conversations[i].Messages, out.Conversations[i].Bytes = pl.Messages, pl.Bytes
	}
	if len(ids) > 0 {
		if out.All, err = plan(ctx, p.Pool, ids, nil); err != nil {
			return nil, err
		}
	}
	prows, err := p.Pool.Query(ctx, `SELECT id, name, kind, archived_at FROM projects
	    WHERE owner_id = $1 AND archived_at IS NOT NULL ORDER BY archived_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	for prows.Next() {
		var a ArchivedProject
		if err := prows.Scan(&a.ID, &a.Name, &a.Kind, &a.ArchivedAt); err != nil {
			prows.Close()
			return nil, err
		}
		out.Projects = append(out.Projects, a)
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return nil, err
	}
	for i := range out.Projects {
		pl, err := p.PlanProject(ctx, out.Projects[i].ID)
		if err != nil {
			return nil, err
		}
		out.Projects[i].Plan = pl
	}
	return out, nil
}

// ArchivedIDs lists the ids "Delete all archived" covers for a caller: the
// same set ListArchived shows, without the cut.
func (p *Purger) ArchivedIDs(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := p.Pool.Query(ctx, `
	    SELECT c.id FROM conversations c JOIN projects pr ON pr.id = c.project_id
	     WHERE c.archived_at IS NOT NULL AND pr.archived_at IS NULL AND (c.user_id = $1 OR pr.owner_id = $1)`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
