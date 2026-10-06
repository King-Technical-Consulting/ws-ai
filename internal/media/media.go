// Package media is the image and video gateway (PLAN.md M6). It gives
// media generation the same shape as text: a job names a selector (an
// endpoint id, a policy alias or auto), the gateway's router picks media
// endpoints for the task class (image or video) under the same policies
// and budgets as text, an engine behind a provider-neutral interface runs
// the job, and the outputs become attachments in the blob store with one
// usage_ledger row per job. Jobs are media_jobs rows driven by the River
// job media.generate, so a gallery card survives a page reload and the
// worker can fail over between endpoints like the text gateway does.
//
// Engines: OpenAI Images (hosted) now; ComfyUI (local), fal.ai and
// Google's Imagen and Veo are planned behind the same interface.
package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
	"github.com/jking323/ws/internal/store/blob"
)

// Job kinds (media_jobs.kind).
const (
	KindImage   = "image"
	KindVideo   = "video"
	KindEdit    = "edit"    // planned: image plus prompt and mask
	KindUpscale = "upscale" // planned
)

// Job statuses (media_jobs.status).
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusDone      = "done"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// MaxImagesPerJob is the hard cap on images per job whatever the endpoint
// declares; hosted image models bill per image.
const MaxImagesPerJob = 4

// ErrInvalid marks a request the caller got wrong (HTTP 400).
var ErrInvalid = errors.New("media: invalid request")

// ErrNoEngine says the routed endpoint names an engine this build lacks.
var ErrNoEngine = errors.New("media: no engine for endpoint")

// Store is the slice of the store the service uses (*store.DB implements it).
type Store interface {
	CreateMediaJob(ctx context.Context, arg store.CreateMediaJobParams) (store.MediaJob, error)
	GetMediaJob(ctx context.Context, id uuid.UUID) (store.MediaJob, error)
	ClaimMediaJob(ctx context.Context, id uuid.UUID) (store.MediaJob, error)
	SetMediaJobProgress(ctx context.Context, arg store.SetMediaJobProgressParams) error
	FinishMediaJob(ctx context.Context, arg store.FinishMediaJobParams) error
	FailMediaJob(ctx context.Context, arg store.FailMediaJobParams) error
	CreateAttachment(ctx context.Context, arg store.CreateAttachmentParams) (store.Attachment, error)
	ListAttachmentsByIDs(ctx context.Context, ids []uuid.UUID) ([]store.Attachment, error)
}

// Inputs is media_jobs.inputs: what the job was asked for. The prompt is
// stored (unlike Claude Code jobs, nothing here is a credential or a
// transcript); EstimateUSD is the price shown before the job ran.
type Inputs struct {
	Prompt  string `json:"prompt"`
	Size    string `json:"size,omitempty"`    // "WxH" or an engine keyword such as "auto"
	N       int    `json:"n,omitempty"`       // images per job
	Quality string `json:"quality,omitempty"` // engine-specific: low | medium | high for OpenAI
	Seconds int    `json:"seconds,omitempty"` // video length
	// SourceAttachmentID is the input image for edit, upscale and
	// image-to-video jobs (planned).
	SourceAttachmentID string  `json:"source_attachment_id,omitempty"`
	EstimateUSD        float64 `json:"estimate_usd,omitempty"`
}

// Request is what an engine runs: the inputs plus the model name the
// endpoint declared. Kind is image or video.
type Request struct {
	Kind    string
	Model   string
	Prompt  string
	Size    string
	N       int
	Quality string
	Seconds int
}

// Output is one generated file.
type Output struct {
	MIME    string
	Data    []byte
	Width   int
	Height  int
	Seconds float64 // video length, 0 for images
}

// Result is what an engine returns for one job.
type Result struct {
	Outputs []Output
	// Usage is token accounting when the engine reports it (OpenAI's image
	// models do); UsageKnown says whether it did. The service prices the
	// job by tokens when both the usage and the endpoint's token rates
	// exist, else per image or per second.
	Usage      gateway.Usage
	UsageKnown bool
	// ProviderJobID is the engine's own id for asynchronous engines.
	ProviderJobID string
	// RevisedPrompt is the prompt the engine actually used, when it says.
	RevisedPrompt string
}

// Progress reports an engine's progress: fraction in [0,1] and the
// provider's job id once known ("" to leave it).
type Progress func(fraction float64, providerJobID string)

// Engine runs media jobs against one kind of provider API. Engines are
// looked up by Capabilities.Media.Engine on the routed endpoint; the
// provider supplies the base URL and key, the endpoint the model name and
// what it can do. Generate blocks until the job is done, polling the
// provider itself when its API is asynchronous.
type Engine interface {
	Generate(ctx context.Context, p *gateway.Provider, ep *gateway.Endpoint, req *Request, progress Progress) (*Result, error)
}

// Service creates and runs media jobs.
type Service struct {
	DB      Store
	Blobs   blob.Store
	GW      *gateway.Gateway
	Engines map[string]Engine
	// Enqueue schedules Run for a new job (River's media.generate). nil
	// leaves jobs queued, which only tests want.
	Enqueue func(ctx context.Context, jobID uuid.UUID) error
	Log     *slog.Logger
	// Now is the clock (tests).
	Now func() time.Time
}

// DefaultEngines returns the engines this build ships.
func DefaultEngines() map[string]Engine {
	return map[string]Engine{EngineOpenAIImages: &OpenAIImages{}}
}

// EngineInfo describes an engine for the admin UI: what a media endpoint
// on it can declare and what the provider behind it must look like.
type EngineInfo struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Image        bool     `json:"image"`
	ImageEdit    bool     `json:"image_edit"`
	Video        bool     `json:"video"`
	ImageToVideo bool     `json:"image_to_video"`
	Sizes        []string `json:"sizes"`         // sizes the engine is known to accept, as a starting point
	ProviderKind string   `json:"provider_kind"` // the provider kind the engine speaks through
	Note         string   `json:"note"`
}

var engineCatalog = map[string]EngineInfo{
	EngineOpenAIImages: {
		ID: EngineOpenAIImages, Name: "OpenAI Images API", Image: true, ImageEdit: false,
		Sizes:        []string{"1024x1024", "1536x1024", "1024x1536", "auto"},
		ProviderKind: string(gateway.ProviderOpenAICompat),
		Note:         "POST /images/generations on the provider's base URL (OpenAI, or any server that mirrors it). gpt-image-1 reports token usage, so set the per-token rates for exact pricing; per_image is the estimate shown before a job runs.",
	},
}

// EngineList lists the engines registered on this service, for the UI.
func (s *Service) EngineList() []EngineInfo {
	out := make([]EngineInfo, 0, len(s.Engines))
	for id := range s.Engines {
		if info, ok := engineCatalog[id]; ok {
			out = append(out, info)
		} else {
			out = append(out, EngineInfo{ID: id, Name: id, Image: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// KnownEngine reports whether an engine id is registered here.
func (s *Service) KnownEngine(id string) bool {
	_, ok := s.Engines[id]
	return ok
}

// CreateParams describe a new job.
type CreateParams struct {
	UserID         uuid.UUID
	ProjectID      uuid.UUID
	ConversationID uuid.NullUUID
	Kind           string
	Selector       string
	Inputs         Inputs
}

// Create validates the request against the endpoint the router would pick,
// prices it, inserts the queued row and enqueues it. The returned row's
// inputs carry estimate_usd.
func (s *Service) Create(ctx context.Context, p CreateParams) (*store.MediaJob, error) {
	in := p.Inputs
	in.Prompt = strings.TrimSpace(in.Prompt)
	if in.Prompt == "" {
		return nil, fmt.Errorf("%w: prompt is required", ErrInvalid)
	}
	if len(in.Prompt) > 32_000 {
		return nil, fmt.Errorf("%w: prompt is too long", ErrInvalid)
	}
	switch p.Kind {
	case "":
		p.Kind = KindImage
	case KindImage, KindVideo:
	default:
		return nil, fmt.Errorf("%w: kind %q is not supported yet", ErrInvalid, p.Kind)
	}
	if p.Selector == "" {
		p.Selector = "auto"
	}
	if in.N <= 0 {
		in.N = 1
	}
	if in.N > MaxImagesPerJob {
		return nil, fmt.Errorf("%w: at most %d images per job", ErrInvalid, MaxImagesPerJob)
	}
	cands, _, err := s.route(ctx, p.UserID, p.ConversationID, p.Selector, p.Kind)
	if err != nil {
		return nil, err
	}
	ep := cands[0].Endpoint
	if err := check(ep, &in, p.Kind); err != nil {
		return nil, err
	}
	in.EstimateUSD = estimate(ep, in, p.Kind)
	raw, _ := json.Marshal(in)
	job, err := s.DB.CreateMediaJob(ctx, store.CreateMediaJobParams{
		UserID: p.UserID, ProjectID: p.ProjectID, ConversationID: p.ConversationID,
		Kind: p.Kind, Selector: p.Selector, Inputs: raw,
	})
	if err != nil {
		return nil, err
	}
	if s.Enqueue != nil {
		if err := s.Enqueue(ctx, job.ID); err != nil {
			msg := "could not enqueue: " + err.Error()
			_ = s.DB.FailMediaJob(ctx, store.FailMediaJobParams{ID: job.ID, Error: &msg})
			return nil, fmt.Errorf("media: enqueue: %w", err)
		}
	}
	return &job, nil
}

// check validates inputs against what the endpoint declares.
func check(ep *gateway.Endpoint, in *Inputs, kind string) error {
	m := ep.Capabilities.Media
	if m == nil {
		return fmt.Errorf("%w: %s is not a media endpoint", ErrInvalid, ep.ID)
	}
	if kind == KindImage {
		max := m.MaxImages
		if max <= 0 {
			max = 1
		}
		if in.N > max {
			return fmt.Errorf("%w: %s makes at most %d image(s) per job", ErrInvalid, ep.ID, max)
		}
	} else if in.N > 1 {
		return fmt.Errorf("%w: one video per job", ErrInvalid)
	}
	if in.Size != "" && len(m.Sizes) > 0 && !containsFold(m.Sizes, in.Size) {
		return fmt.Errorf("%w: %s accepts sizes %s", ErrInvalid, ep.ID, strings.Join(m.Sizes, ", "))
	}
	if kind == KindVideo && m.MaxSeconds > 0 && in.Seconds > m.MaxSeconds {
		return fmt.Errorf("%w: %s makes at most %d seconds", ErrInvalid, ep.ID, m.MaxSeconds)
	}
	return nil
}

// Estimate prices a job on an endpoint before it runs.
func estimate(ep *gateway.Endpoint, in Inputs, kind string) float64 {
	if ep.Local {
		return 0
	}
	if kind == KindVideo {
		return ep.Pricing.MediaCost(0, float64(in.Seconds))
	}
	return ep.Pricing.MediaCost(in.N, 0)
}

// Estimate is estimate for callers outside the package (the models list).
func Estimate(ep *gateway.Endpoint, in Inputs, kind string) float64 { return estimate(ep, in, kind) }

// route asks the gateway for media candidates: the budget middleware and
// the policies apply as for text, and the task class keeps text endpoints
// out.
func (s *Service) route(ctx context.Context, userID uuid.UUID, conv uuid.NullUUID, selector, kind string) ([]gateway.Candidate, gateway.Decision, error) {
	tc := gateway.TaskImage
	if kind == KindVideo {
		tc = gateway.TaskVideo
	}
	req := &gateway.Request{Model: selector, Metadata: gateway.Metadata{UserID: userID.String(), TaskClass: tc}}
	if conv.Valid {
		req.Metadata.ConversationID = conv.UUID.String()
	}
	cands, dec, err := s.GW.Prepare(ctx, req)
	if err != nil {
		if errors.Is(err, gateway.ErrBudgetExceeded) {
			return nil, dec, err
		}
		return nil, dec, fmt.Errorf("%w: no %s endpoint for %q: %v", ErrInvalid, kind, selector, strings.TrimPrefix(err.Error(), gateway.ErrNoRoute.Error()+": "))
	}
	return cands, dec, nil
}

// Run drives one queued job to done or failed. It is the body of the
// media.generate River job and never returns an error for a job-level
// failure (that is recorded on the row); only an unclaimable job that
// looks abandoned is repaired here.
func (s *Service) Run(ctx context.Context, id uuid.UUID) error {
	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	job, err := s.DB.ClaimMediaJob(ctx, id)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// Not queued: done, cancelled, or left running by a process that
		// died. River rescues stuck jobs after a while; mark the row then.
		cur, gerr := s.DB.GetMediaJob(ctx, id)
		if gerr == nil && cur.Status == StatusRunning && cur.StartedAt != nil && s.now().Sub(*cur.StartedAt) > 15*time.Minute {
			msg := "abandoned: the worker running it stopped"
			_ = s.DB.FailMediaJob(ctx, store.FailMediaJobParams{ID: id, Error: &msg})
		}
		return nil
	}
	var in Inputs
	_ = json.Unmarshal(job.Inputs, &in)
	start := s.now()
	meta := gateway.Metadata{UserID: job.UserID.String(), TaskClass: gateway.TaskImage}
	if job.Kind == KindVideo {
		meta.TaskClass = gateway.TaskVideo
	}
	if job.ConversationID.Valid {
		meta.ConversationID = job.ConversationID.UUID.String()
	}
	fail := func(ep string, err error) {
		msg := err.Error()
		var epp *string
		if ep != "" {
			epp = &ep
		}
		_ = s.DB.FailMediaJob(ctx, store.FailMediaJobParams{ID: job.ID, EndpointID: epp, Error: &msg})
		log.Warn("media: job failed", "job", job.ID, "kind", job.Kind, "endpoint", ep, "err", msg)
	}
	cands, dec, err := s.route(ctx, job.UserID, job.ConversationID, job.Selector, job.Kind)
	if err != nil {
		fail("", err)
		return nil
	}
	req := &Request{Kind: job.Kind, Prompt: in.Prompt, Size: in.Size, N: in.N, Quality: in.Quality, Seconds: in.Seconds}
	if req.N <= 0 {
		req.N = 1
	}
	var lastErr error
	var lastEP string
	for _, c := range cands {
		ep := c.Endpoint
		eng := s.Engines[ep.Capabilities.Media.Engine]
		if eng == nil {
			lastErr = fmt.Errorf("%w: %s wants %q", ErrNoEngine, ep.ID, ep.Capabilities.Media.Engine)
			lastEP = ep.ID
			continue
		}
		dec.Tried = append(dec.Tried, ep.ID)
		if err := check(ep, &in, job.Kind); err != nil {
			lastErr, lastEP = err, ep.ID
			continue
		}
		req.Model = ep.ModelName
		progress := func(f float64, pid string) {
			var pidp *string
			if pid != "" {
				pidp = &pid
			}
			_ = s.DB.SetMediaJobProgress(context.WithoutCancel(ctx), store.SetMediaJobProgressParams{ID: job.ID, Progress: float32(f), ProviderJobID: pidp})
		}
		res, err := eng.Generate(ctx, c.Provider, ep, req, progress)
		if err != nil {
			lastErr, lastEP = err, ep.ID
			log.Warn("media: engine failed, trying next", "job", job.ID, "endpoint", ep.ID, "err", err)
			if ctx.Err() != nil {
				break
			}
			continue
		}
		if len(res.Outputs) == 0 {
			lastErr, lastEP = errors.New("engine returned no output"), ep.ID
			continue
		}
		ids, err := s.store(ctx, job, res.Outputs)
		if err != nil {
			fail(ep.ID, fmt.Errorf("store outputs: %w", err))
			return nil
		}
		cost, usage := s.price(ep, res)
		epID := ep.ID
		if err := s.DB.FinishMediaJob(ctx, store.FinishMediaJobParams{ID: job.ID, EndpointID: &epID, OutputAttachmentIds: ids, CostUsd: cost}); err != nil {
			log.Error("media: finish", "job", job.ID, "err", err)
		}
		dec.Chosen = ep.ID
		s.GW.Record(ctx, gateway.UsageRecord{Metadata: meta, EndpointID: ep.ID, Model: ep.ModelName, Decision: dec,
			Usage: usage, CostUSD: cost, Latency: s.now().Sub(start), FinishReason: gateway.FinishStop})
		log.Info("media: job done", "job", job.ID, "kind", job.Kind, "endpoint", ep.ID, "outputs", len(ids), "cost_usd", cost)
		return nil
	}
	if lastErr == nil {
		lastErr = errors.New("no media endpoint could take the job")
	}
	fail(lastEP, lastErr)
	s.GW.Record(ctx, gateway.UsageRecord{Metadata: meta, EndpointID: lastEP, Decision: dec, Latency: s.now().Sub(start), FinishReason: gateway.FinishError, Err: lastErr.Error()})
	return nil
}

// price returns the job's cost and the usage to put on the ledger.
func (s *Service) price(ep *gateway.Endpoint, res *Result) (float64, gateway.Usage) {
	if ep.Local {
		return 0, res.Usage
	}
	if res.UsageKnown && (ep.Pricing.InputPerM > 0 || ep.Pricing.OutputPerM > 0) {
		return ep.Pricing.Cost(res.Usage), res.Usage
	}
	var secs float64
	for _, o := range res.Outputs {
		secs += o.Seconds
	}
	return ep.Pricing.MediaCost(len(res.Outputs), secs), res.Usage
}

// store writes outputs to the blob store and creates attachment rows.
func (s *Service) store(ctx context.Context, job store.MediaJob, outs []Output) ([]uuid.UUID, error) {
	ids := make([]uuid.UUID, 0, len(outs))
	for i, o := range outs {
		key, err := blob.PutBytes(ctx, s.Blobs, o.Data)
		if err != nil {
			return nil, err
		}
		sum, _ := hex.DecodeString(strings.TrimPrefix(key, "sha256/"))
		if len(sum) != sha256.Size {
			h := sha256.Sum256(o.Data)
			sum = h[:]
		}
		var w, h *int32
		if o.Width > 0 && o.Height > 0 {
			ww, hh := int32(o.Width), int32(o.Height)
			w, h = &ww, &hh
		}
		mime := o.MIME
		if mime == "" {
			mime = "application/octet-stream"
		}
		att, err := s.DB.CreateAttachment(ctx, store.CreateAttachmentParams{
			UserID: job.UserID, BlobKey: key, Mime: mime, Bytes: int64(len(o.Data)), Sha256: sum,
			Filename: fmt.Sprintf("%s-%s-%d%s", job.Kind, job.ID.String()[:8], i+1, extFor(mime)), Width: w, Height: h,
		})
		if err != nil {
			return nil, err
		}
		ids = append(ids, att.ID)
	}
	return ids, nil
}

// Outputs returns a job's attachments in output order.
func (s *Service) Outputs(ctx context.Context, job *store.MediaJob) ([]store.Attachment, error) {
	if len(job.OutputAttachmentIds) == 0 {
		return nil, nil
	}
	rows, err := s.DB.ListAttachmentsByIDs(ctx, job.OutputAttachmentIds)
	if err != nil {
		return nil, err
	}
	byID := make(map[uuid.UUID]store.Attachment, len(rows))
	for _, a := range rows {
		byID[a.ID] = a
	}
	out := make([]store.Attachment, 0, len(rows))
	for _, id := range job.OutputAttachmentIds {
		if a, ok := byID[id]; ok {
			out = append(out, a)
		}
	}
	return out, nil
}

// Wait polls a job until it leaves queued and running, the context ends,
// or timeout passes. It returns the last row seen.
func (s *Service) Wait(ctx context.Context, id uuid.UUID, timeout time.Duration, poll time.Duration) (*store.MediaJob, error) {
	if poll <= 0 {
		poll = time.Second
	}
	deadline := s.now().Add(timeout)
	for {
		job, err := s.DB.GetMediaJob(ctx, id)
		if err != nil {
			return nil, err
		}
		if job.Status != StatusQueued && job.Status != StatusRunning {
			return &job, nil
		}
		if s.now().After(deadline) {
			return &job, fmt.Errorf("media: job %s still %s after %s", id, job.Status, timeout)
		}
		select {
		case <-ctx.Done():
			return &job, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// Endpoints lists the enabled media endpoints of a kind (image or video),
// for pickers.
func (s *Service) Endpoints(kind string) []*gateway.Endpoint {
	var out []*gateway.Endpoint
	for _, e := range s.GW.Registry.Endpoints() {
		m := e.Capabilities.Media
		if !e.Enabled || m == nil {
			continue
		}
		switch kind {
		case KindImage:
			if !m.Image {
				continue
			}
		case KindVideo:
			if !m.Video {
				continue
			}
		}
		out = append(out, e)
	}
	return out
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

func extFor(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	}
	return ""
}
