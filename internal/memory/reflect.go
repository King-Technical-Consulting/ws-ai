package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"

	"github.com/jking323/ws/internal/agents"
	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

// ReflectResult is what one reflection did.
type ReflectResult struct {
	Conversations int `json:"conversations"`
	Added         int `json:"added"`
	Merged        int `json:"merged"`
	Demoted       int `json:"demoted"`
}

type extracted struct {
	Facts       []item `json:"facts"`
	Preferences []item `json:"preferences"`
	Decisions   []item `json:"decisions"`
}

type item struct {
	Content    string  `json:"content"`
	Importance float32 `json:"importance"`
}

const reflectSystem = `You maintain a person's long-term memory in an AI workspace. From the conversations below, extract what is worth remembering about THE PERSON for future chats. Reply with JSON only, matching:
{"facts":[{"content":string,"importance":number}],"preferences":[{"content":string,"importance":number}],"decisions":[{"content":string,"importance":number}]}
Facts are stable things about the person or their work: who they are, what they use, what they are building, names that recur. Preferences are how they want to be answered or how they like things done. Decisions are choices they made that later chats should not reopen. Each item is one short, self-contained sentence. Importance is 0 to 1. Skip anything already in the existing memories, one-off questions, pleasantries, secrets and credentials, and anything about other people that the person would not want kept. At most six facts, four preferences and three decisions. If nothing is worth keeping, return empty lists.`

// Reflect reads the member's conversations since the last reflection, adds
// what is new, merges what is the same as an existing memory, and demotes
// memories nobody has used for a while. It never deletes. It is the body of
// the memory.reflect job and of the "tidy up now" button.
func (s *Service) Reflect(ctx context.Context, userID uuid.UUID) (ReflectResult, error) {
	var res ReflectResult
	set, err := s.Settings(ctx, userID)
	if err != nil {
		return res, err
	}
	if !set.Enabled {
		return res, nil
	}
	now := s.now()
	since := now.Add(-24 * time.Hour)
	if set.LastReflectedAt != nil {
		since = *set.LastReflectedAt
	}
	convs, err := s.DB.ListConversationsActiveSince(ctx, store.ListConversationsActiveSinceParams{UserID: userID, UpdatedAt: since, Limit: 20})
	if err != nil {
		return res, err
	}
	var transcript strings.Builder
	for _, c := range convs {
		rows, err := s.DB.ListMessages(ctx, c.ID)
		if err != nil {
			continue
		}
		t := agents.Transcript(rows, 3500)
		if strings.TrimSpace(t) == "" {
			continue
		}
		res.Conversations++
		fmt.Fprintf(&transcript, "### Conversation: %s\n%s\n\n", c.Title, t)
		if transcript.Len() > 20_000 {
			break
		}
	}
	if res.Conversations > 0 {
		existing, _ := s.DB.TopUserMemories(ctx, store.TopUserMemoriesParams{UserID: userID, Limit: 40})
		var ex strings.Builder
		for _, m := range existing {
			fmt.Fprintf(&ex, "- [%s] %s\n", m.Kind, m.Content)
		}
		resp, err := s.GW.Complete(ctx, &gateway.Request{
			Model:  "auto",
			System: reflectSystem,
			Messages: []gateway.Message{{Role: gateway.RoleUser, Parts: []gateway.Part{
				gateway.TextPart("Existing memories:\n" + orNone(ex.String()) + "\nConversations:\n" + transcript.String()),
			}}},
			MaxTokens: 1500,
			JSON:      &gateway.JSONFormat{},
			Metadata:  gateway.Metadata{UserID: userID.String(), TaskClass: gateway.TaskReflect},
		})
		if err != nil {
			return res, fmt.Errorf("memory: reflect: %w", err)
		}
		var out extracted
		text := strings.TrimSpace(resp.Text())
		if i, j := strings.Index(text, "{"), strings.LastIndex(text, "}"); i >= 0 && j > i {
			_ = json.Unmarshal([]byte(text[i:j+1]), &out)
		}
		type kept struct {
			kind string
			item
		}
		var all []kept
		for _, f := range out.Facts {
			all = append(all, kept{KindFact, f})
		}
		for _, f := range out.Preferences {
			all = append(all, kept{KindPreference, f})
		}
		for _, f := range out.Decisions {
			all = append(all, kept{KindDecision, f})
		}
		var good []kept
		for _, k := range all {
			k.Content = strings.TrimSpace(k.Content)
			if k.Content == "" || len(k.Content) > MaxContent {
				continue
			}
			if k.Importance <= 0 || k.Importance > 1 {
				k.Importance = 0.5
			}
			good = append(good, k)
		}
		if len(good) > 0 {
			texts := make([]string, len(good))
			for i, g := range good {
				texts[i] = g.Content
			}
			vecs, err := s.embed(ctx, userID, texts)
			if err != nil {
				s.log().Warn("memory: reflect embed failed, storing without vectors", "err", err)
				vecs = make([]*pgvector.Vector, len(good))
			}
			count, _ := s.DB.CountUserMemories(ctx, userID)
			for i, g := range good {
				merged, err := s.mergeIfSame(ctx, userID, g.Content, g.Importance, vecs[i])
				if err != nil {
					s.log().Warn("memory: reflect merge check", "err", err)
				}
				if merged {
					res.Merged++
					continue
				}
				if count >= MaxPerUser {
					continue
				}
				row, err := s.DB.UpsertUserMemory(ctx, store.UpsertUserMemoryParams{
					UserID: userID, Kind: g.kind, Content: g.Content, ContentHash: hashOf(g.Content), Embedding: vecs[i], Source: SourceReflection, Importance: g.Importance,
				})
				if err != nil {
					s.log().Warn("memory: reflect upsert", "err", err)
					continue
				}
				_ = row
				count++
				res.Added++
			}
		}
	}
	n, err := s.DB.DecayUserMemories(ctx, store.DecayUserMemoriesParams{UserID: userID, Cutoff: now.Add(-staleAfter), Factor: decayFactor})
	if err != nil {
		s.log().Warn("memory: decay", "err", err)
	}
	res.Demoted = int(n)
	nowv := now
	if err := s.DB.MarkMemoryReflected(ctx, store.MarkMemoryReflectedParams{UserID: userID, LastReflectedAt: &nowv}); err != nil {
		return res, err
	}
	s.log().Info("memory: reflected", "conversations", res.Conversations, "added", res.Added, "merged", res.Merged, "demoted", res.Demoted)
	return res, nil
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)\n"
	}
	return s
}

// mergeIfSame folds a new statement into an existing memory when one is
// near enough in meaning: the existing text stays, its importance rises to
// the new one's if higher. Without a vector there is nothing to compare
// beyond the exact-text match the upsert already does.
func (s *Service) mergeIfSame(ctx context.Context, userID uuid.UUID, content string, importance float32, vec *pgvector.Vector) (bool, error) {
	if vec == nil {
		return false, nil
	}
	rows, err := s.DB.SearchUserMemories(ctx, store.SearchUserMemoriesParams{UserID: userID, Column2: *vec, Limit: 1})
	if err != nil || len(rows) == 0 {
		return false, err
	}
	near := rows[0]
	if near.Distance > mergeDistance {
		return false, nil
	}
	if importance > near.Importance {
		_, err = s.DB.UpdateUserMemory(ctx, store.UpdateUserMemoryParams{
			ID: near.ID, UserID: userID, Content: near.Content, ContentHash: near.ContentHash, Kind: near.Kind, Importance: importance, Embedding: near.Embedding,
		})
	}
	return true, err
}

// Due returns the members whose nightly reflection is due at now: reflection
// is on, today's slot in their time zone has passed, and they have not been
// reflected since that slot.
func (s *Service) Due(ctx context.Context, now time.Time) ([]uuid.UUID, error) {
	rows, err := s.DB.ListReflectingMembers(ctx)
	if err != nil {
		return nil, err
	}
	var out []uuid.UUID
	for _, r := range rows {
		if isDue(r.ReflectAt, r.Timezone, r.LastReflectedAt, now) {
			out = append(out, r.UserID)
		}
	}
	return out, nil
}

func isDue(at, tz string, last *time.Time, now time.Time) bool {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	t, err := time.Parse("15:04", at)
	if err != nil {
		return false
	}
	local := now.In(loc)
	slot := time.Date(local.Year(), local.Month(), local.Day(), t.Hour(), t.Minute(), 0, 0, loc)
	if local.Before(slot) {
		return false
	}
	return last == nil || last.Before(slot)
}

// Tick enqueues a reflection for every member who is due; the job layer
// calls it every few minutes.
func (s *Service) Tick(ctx context.Context, enqueue func(context.Context, uuid.UUID) error) error {
	due, err := s.Due(ctx, s.now())
	if err != nil {
		return err
	}
	for _, id := range due {
		if err := enqueue(ctx, id); err != nil {
			s.log().Warn("memory: enqueue reflection", "err", err)
		}
	}
	return nil
}

// ReflectJob is Reflect for the job layer, which has no use for the counts.
func (s *Service) ReflectJob(ctx context.Context, userID uuid.UUID) error {
	_, err := s.Reflect(ctx, userID)
	return err
}

// TickDue is Tick under the name the job layer's interface uses.
func (s *Service) TickDue(ctx context.Context, enqueue func(context.Context, uuid.UUID) error) error {
	return s.Tick(ctx, enqueue)
}
