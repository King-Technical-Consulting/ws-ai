package media

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestUpscaleJob: an upscale needs a source and an endpoint that upscales,
// takes no prompt, forces one output, validates the scale, and hands the
// engine the source and the factor.
func TestUpscaleJob(t *testing.T) {
	eng := &fakeEngine{}
	ep := imageEndpoint("fal/img", "fake", 0.03)
	ep.Capabilities.Media.Image = false
	ep.Capabilities.Media.Upscale = true
	gen := imageEndpoint("openai/img", "fake", 0.04)
	svc, db := newService(t, &memRecorder{}, map[string]Engine{"fake": eng}, gen, ep)
	ctx := context.Background()
	uid, pid := uuid.New(), uuid.New()
	att := putImage(t, svc, db, uid, "image/png")

	if _, err := svc.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Kind: KindUpscale, Inputs: Inputs{}}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "source") {
		t.Errorf("upscale without source: %v", err)
	}
	if _, err := svc.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Kind: KindUpscale, Inputs: Inputs{SourceAttachmentID: att.ID.String(), Scale: 3}}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "scale") {
		t.Errorf("bad scale: %v", err)
	}
	job, err := svc.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Kind: KindUpscale, Inputs: Inputs{SourceAttachmentID: att.ID.String(), N: 3}})
	if err != nil {
		t.Fatal(err)
	}
	var in Inputs
	if err := json.Unmarshal(job.Inputs, &in); err != nil {
		t.Fatal(err)
	}
	if in.Scale != 2 || in.N != 1 || in.EstimateUSD != 0.03 {
		t.Errorf("inputs = %+v", in)
	}
	if err := svc.Run(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetMediaJob(ctx, job.ID)
	if got.Status != StatusDone || eng.last == nil || eng.last.Kind != KindUpscale || eng.last.Scale != 2 || eng.last.Source == nil || *got.EndpointID != "fal/img" {
		t.Errorf("job = %+v last = %+v", got, eng.last)
	}
	// Only an edit takes a mask; the mask must be an image the caller may use.
	if _, err := svc.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Kind: KindUpscale, Inputs: Inputs{SourceAttachmentID: att.ID.String(), MaskAttachmentID: att.ID.String()}}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "mask") {
		t.Errorf("mask on upscale: %v", err)
	}
	// Without an upscaling endpoint the job is refused up front.
	ep.Capabilities.Media.Upscale = false
	if _, err := svc.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Kind: KindUpscale, Inputs: Inputs{SourceAttachmentID: att.ID.String()}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("no upscale endpoint: %v", err)
	}
}

// TestEditWithMask: a mask is validated like a source and reaches the
// engine alongside it.
func TestEditWithMask(t *testing.T) {
	eng := &fakeEngine{}
	ep := imageEndpoint("openai/img", "fake", 0.04)
	ep.Capabilities.Media.ImageEdit = true
	svc, db := newService(t, &memRecorder{}, map[string]Engine{"fake": eng}, ep)
	ctx := context.Background()
	uid, pid := uuid.New(), uuid.New()
	att := putImage(t, svc, db, uid, "image/png")
	mask := putImage(t, svc, db, uid, "image/png")
	other := putImage(t, svc, db, uuid.New(), "image/png")

	if _, err := svc.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Inputs: Inputs{Prompt: "x", SourceAttachmentID: att.ID.String(), MaskAttachmentID: other.ID.String()}}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "mask") {
		t.Errorf("foreign mask: %v", err)
	}
	job, err := svc.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Inputs: Inputs{Prompt: "sky only", SourceAttachmentID: att.ID.String(), MaskAttachmentID: mask.ID.String()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Run(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if eng.last == nil || eng.last.Kind != KindEdit || eng.last.Mask == nil || eng.last.Mask.Width != 6 {
		t.Errorf("last = %+v", eng.last)
	}
}
