package training

// Training from a recipe (PLAN.md M12, cut c): the merge. A fine-tune job
// that starts from a recipe builds its dataset first: the recipe's
// attached datasets are read, an uploaded one is mapped again under the
// spec's mapping when that differs from how it was built, the spec's
// system prompt is put on every example, and the result is stored as a new
// dataset of kind "merged" that the job then trains on. The trainer scripts
// see ordinary chat JSONL; the eval gate finds its held-out set where it
// always does.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/store"
	"github.com/jking323/ws/internal/store/blob"
)

// MergeSource is what one attached dataset contributed.
type MergeSource struct {
	DatasetID string `json:"dataset_id"`
	Name      string `json:"name"`
	Examples  int    `json:"examples"`
	Eval      int    `json:"eval"`
	// Remapped is set when the spec's mapping differed from how the upload
	// was built and the upload was read again under the spec's.
	Remapped bool   `json:"remapped,omitempty"`
	Skipped  string `json:"skipped,omitempty"`
}

// MergeReport is stored on the merged dataset.
type MergeReport struct {
	RecipeID      string        `json:"recipe_id"`
	Sources       []MergeSource `json:"sources"`
	SystemPrompt  bool          `json:"system_prompt"`
	Truncated     bool          `json:"truncated,omitempty"`
	TrainExamples int           `json:"train"`
	EvalExamples  int           `json:"eval"`
}

// withSystem puts system as the first message of an example, replacing a
// system message that is already first. An empty system leaves it alone.
func withSystem(ex *Example, system string) {
	if system == "" {
		return
	}
	if len(ex.Messages) > 0 && ex.Messages[0].Role == "system" {
		ex.Messages[0].Content = system
		return
	}
	ex.Messages = append([]ChatMessage{{Role: "system", Content: system}}, ex.Messages...)
}

// appendWithSystem copies JSONL examples from r into dst under system,
// stopping after limit examples; it returns how many it wrote.
func appendWithSystem(dst *bytes.Buffer, r io.Reader, system string, limit int) (int, error) {
	dec := json.NewDecoder(bufio.NewReaderSize(r, 1<<20))
	enc := json.NewEncoder(dst)
	enc.SetEscapeHTML(false)
	n := 0
	for n < limit {
		var ex Example
		if err := dec.Decode(&ex); err != nil {
			if err == io.EOF {
				return n, nil
			}
			return n, err
		}
		withSystem(&ex, system)
		if err := enc.Encode(ex); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// specMapping returns the spec's mapping for a dataset when it names an
// input and an output.
func specMapping(sp Spec, id uuid.UUID) (SpecMapping, bool) {
	for _, m := range sp.Mappings {
		if m.DatasetID == id.String() && m.Input != "" && m.Output != "" {
			return m, true
		}
	}
	return SpecMapping{}, false
}

// mergeRecipe builds the merged dataset for a recipe job and returns its id.
func (s *Service) mergeRecipe(ctx context.Context, job store.FinetuneJob) (uuid.UUID, error) {
	if !job.RecipeID.Valid {
		return uuid.Nil, errors.New("the job has no recipe")
	}
	rec, err := s.DB.GetRecipe(ctx, job.RecipeID.UUID)
	if err != nil {
		return uuid.Nil, errors.New("the recipe was deleted")
	}
	var sp Spec
	_ = json.Unmarshal(job.Spec, &sp)
	rep := MergeReport{RecipeID: rec.ID.String(), SystemPrompt: sp.SystemPrompt != ""}
	var train, eval bytes.Buffer
	room := uploadMaxExamples
	for _, id := range rec.DatasetIds {
		ds, err := s.DB.GetDataset(ctx, id)
		if err != nil {
			rep.Sources = append(rep.Sources, MergeSource{DatasetID: id.String(), Skipped: "deleted"})
			continue
		}
		src := MergeSource{DatasetID: id.String(), Name: ds.Name}
		if ds.Status != StatusReady || ds.BlobKey == nil {
			src.Skipped = "not ready (" + ds.Status + ")"
			rep.Sources = append(rep.Sources, src)
			continue
		}
		if room <= 0 {
			src.Skipped = "over the example cap"
			rep.Truncated = true
			rep.Sources = append(rep.Sources, src)
			continue
		}
		var built Report
		_ = json.Unmarshal(ds.Report, &built)
		var nt, ne int
		if m, ok := specMapping(sp, id); ok && ds.Kind == KindUploaded && differs(m, built.Mapping) {
			// Read the upload again under the spec's mapping; the dataset itself is not changed.
			over := Mapping{}
			_ = json.Unmarshal(ds.Mapping, &over)
			over.Input, over.Output, over.System, over.Split = m.Input, m.Output, m.System, m.Split
			var f Filters
			_ = json.Unmarshal(ds.Filters, &f)
			f.MaxExamples = room
			f.defaults()
			b, err := s.mapUploaded(ctx, ds, over, f)
			if b == nil || err != nil {
				return uuid.Nil, fmt.Errorf("dataset %q under the recipe's mapping: %v", ds.Name, err)
			}
			if nt, err = appendWithSystem(&train, bytes.NewReader(b.out.train.Bytes()), sp.SystemPrompt, room); err != nil {
				return uuid.Nil, err
			}
			if ne, err = appendWithSystem(&eval, bytes.NewReader(b.out.eval.Bytes()), sp.SystemPrompt, room); err != nil {
				return uuid.Nil, err
			}
			src.Remapped = true
		} else {
			rc, err := s.Blobs.Get(ctx, *ds.BlobKey)
			if err != nil {
				return uuid.Nil, fmt.Errorf("dataset %q: %w", ds.Name, err)
			}
			nt, err = appendWithSystem(&train, rc, sp.SystemPrompt, room)
			rc.Close()
			if err != nil {
				return uuid.Nil, fmt.Errorf("dataset %q: %w", ds.Name, err)
			}
			if ds.EvalBlobKey != nil {
				rc, err := s.Blobs.Get(ctx, *ds.EvalBlobKey)
				if err != nil {
					return uuid.Nil, fmt.Errorf("dataset %q: %w", ds.Name, err)
				}
				ne, err = appendWithSystem(&eval, rc, sp.SystemPrompt, room)
				rc.Close()
				if err != nil {
					return uuid.Nil, fmt.Errorf("dataset %q: %w", ds.Name, err)
				}
			}
		}
		src.Examples, src.Eval = nt, ne
		room -= nt + ne
		rep.Sources = append(rep.Sources, src)
		rep.TrainExamples += nt
		rep.EvalExamples += ne
	}
	if rep.TrainExamples == 0 {
		return uuid.Nil, errors.New("the recipe's datasets hold no training examples (none is ready, or every one is empty)")
	}
	raw, _ := json.Marshal(rep)
	ds, err := s.DB.CreateMergedDataset(ctx, store.CreateMergedDatasetParams{OwnerID: job.OwnerID, Name: clip(fmt.Sprintf("%s (merged for %s)", rec.Name, job.AdapterName), 120), Report: raw})
	if err != nil {
		return uuid.Nil, err
	}
	key, err := blob.PutBytes(ctx, s.Blobs, train.Bytes())
	if err != nil {
		return uuid.Nil, err
	}
	var evalKey *string
	if eval.Len() > 0 {
		k, err := blob.PutBytes(ctx, s.Blobs, eval.Bytes())
		if err != nil {
			return uuid.Nil, err
		}
		evalKey = &k
	}
	if err := s.DB.FinishDataset(ctx, store.FinishDatasetParams{ID: ds.ID, BlobKey: &key, EvalBlobKey: evalKey, Bytes: int64(train.Len()), Examples: int32(rep.TrainExamples), EvalExamples: int32(rep.EvalExamples)}); err != nil {
		return uuid.Nil, err
	}
	if err := s.DB.SetFinetuneDataset(ctx, store.SetFinetuneDatasetParams{ID: job.ID, DatasetID: store.NullUUID(ds.ID)}); err != nil {
		return uuid.Nil, err
	}
	s.log().Info("training: recipe merged", "job", job.ID, "dataset", ds.ID, "train", rep.TrainExamples, "eval", rep.EvalExamples, "sources", len(rep.Sources))
	return ds.ID, nil
}

// differs reports whether the spec's mapping names other columns than the
// ones the dataset was built with.
func differs(m SpecMapping, built Mapping) bool {
	return !strings.EqualFold(m.Input, built.Input) || !strings.EqualFold(m.Output, built.Output) ||
		m.System != built.System || m.Split != built.Split
}
