package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/sandbox"
	"github.com/jking323/ws/internal/store"
	"github.com/jking323/ws/internal/store/blob"
)

// SkillOpen is the worker tool that places an approved skill's files in the
// run's sandbox, under SkillBaseDir. It runs nothing and writes no exec
// bit: a script runs only when the agent calls bash, under bash's policy.
// Outside a code project, with no sandbox, it says the scripts are
// unavailable.
type SkillOpen struct {
	DB interface {
		GetConversation(ctx context.Context, id uuid.UUID) (store.Conversation, error)
	}
	// Skills lists the approved skills of an agent.
	Skills  func(ctx context.Context, agentID uuid.UUID) ([]StoredSkill, error)
	Blobs   blob.Store
	M       *sandbox.Manager
	Resolve sandbox.Resolver
}

func (t *SkillOpen) Def() gateway.ToolDef {
	return gateway.ToolDef{
		Name:        SkillOpenTool,
		Description: "Place the files of an approved skill (its scripts, references and assets) in the sandbox under /home/dev/.ws-skills/<name>, so its instructions' {baseDir} paths work. It runs nothing: run a script afterwards with the bash tool.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"The skill's name, as listed in the system prompt"}},"required":["name"]}`),
	}
}
func (t *SkillOpen) DefaultPolicy() agent.Policy { return agent.PolicyAuto }
func (t *SkillOpen) Idempotent() bool            { return true }

func (t *SkillOpen) Call(ctx context.Context, tc agent.ToolCtx, args json.RawMessage) (agent.Result, error) {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return agent.ErrorResult(err), nil
	}
	conv, err := t.DB.GetConversation(ctx, tc.ConversationID)
	if err != nil {
		return agent.ErrorResult(err), nil
	}
	if !conv.AgentID.Valid {
		return agent.ErrorResult(errors.New("skill_open is for agents that have approved skills")), nil
	}
	have, err := t.Skills(ctx, conv.AgentID.UUID)
	if err != nil {
		return agent.ErrorResult(err), nil
	}
	var skill *StoredSkill
	for _, k := range have {
		if k.Name == strings.TrimSpace(in.Name) {
			k := k
			skill = &k
		}
	}
	if skill == nil {
		return agent.ErrorResult(fmt.Errorf("no approved skill named %q", in.Name)), nil
	}
	sb, err := t.Resolve(ctx, tc)
	if err != nil {
		return agent.ErrorResult(fmt.Errorf("scripts unavailable: %v", err)), nil
	}
	base := SkillBaseDir(skill.Name)
	var placed []string
	for _, f := range skill.Files {
		if f.Refused {
			continue
		}
		rel := path.Clean(f.Path)
		if rel == "." || path.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
			return agent.ErrorResult(fmt.Errorf("skill file path %q is not inside the skill folder", f.Path)), nil
		}
		rc, err := t.Blobs.Get(ctx, "sha256/"+f.SHA256)
		if err != nil {
			return agent.ErrorResult(fmt.Errorf("skill file %s is missing from storage", f.Path)), nil
		}
		data, err := io.ReadAll(io.LimitReader(rc, MaxSkillFileBytes+1))
		rc.Close()
		if err != nil || !VerifyBlob(f, data) {
			return agent.ErrorResult(fmt.Errorf("skill file %s does not match the hash that was approved; it was not placed", f.Path)), nil
		}
		if err := t.M.WriteFile(ctx, sb, path.Join(base, rel), data); err != nil {
			return agent.ErrorResult(err), nil
		}
		placed = append(placed, rel)
	}
	return agent.Result{Text: fmt.Sprintf("Placed %d file(s) of %q under %s:\n%s\nNothing was run. Use the bash tool to run a script.", len(placed), skill.Title, base, strings.Join(placed, "\n")), Data: map[string]any{"skill": skill.Name, "dir": base, "files": placed}}, nil
}
