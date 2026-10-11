package training

// Uploaded datasets (PLAN.md M12, cut a). The owner uploads a file or an
// archive of files; the upload is kept as is in the blob store and the
// training.build job maps it into the same chat-format JSONL the
// conversation exports use, so everything downstream (fine-tune jobs, the
// eval gate, previews, downloads) is unchanged.
//
// Accepted files, by extension:
//
//	.jsonl .ndjson  one JSON object per line: a chat example ({"messages":[...]})
//	                as is, or a record read through the mapping
//	.csv .tsv       a header row, then records read through the mapping
//	.txt .md        a document, cut into chunks that continue each other
//	.zip .tar.gz .tgz .tar   any of the above, one level of folders or many
//
// Anything else in an archive is skipped and counted in the report.

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/store"
	"github.com/jking323/ws/internal/store/blob"
)

// KindChat and KindUploaded are the dataset kinds.
const (
	KindChat     = "chat"
	KindUploaded = "uploaded"
)

// DefaultUploadMax is the upload size limit when none is configured.
const DefaultUploadMax int64 = 4 << 30

const (
	// uploadExpand bounds what an archive may unpack to, as a multiple of
	// the upload limit (a zip bomb stops here).
	uploadExpand = 8
	// uploadMaxExamples is the example cap for an uploaded dataset
	// (conversation exports keep their smaller default).
	uploadMaxExamples = 100000
	// maxExampleBytes drops a record whose text is larger than this.
	maxExampleBytes = 1 << 20
	// maxReportFiles bounds the per-file list kept in the report.
	maxReportFiles = 200
	maxWarnings    = 8
	defaultChunk   = 3000
	minChunk       = 200
	maxChunk       = 20000
	contextTail    = 800
)

// Mapping says how records become examples. Input and Output name a CSV
// column or a JSON field (a dotted path reaches into objects, "a.b"); an
// empty name is detected from common ones (prompt, question, completion,
// answer, ...). System names an optional column or field holding a system
// prompt per record; SystemText is one system prompt for every record
// (used when System is empty). Split names an optional column or field
// that pins a record to "train" or "eval". ChunkChars is the target size
// of a text document's chunks.
type Mapping struct {
	Input      string `json:"input,omitempty"`
	Output     string `json:"output,omitempty"`
	System     string `json:"system,omitempty"`
	SystemText string `json:"system_text,omitempty"`
	Split      string `json:"split,omitempty"`
	ChunkChars int    `json:"chunk_chars,omitempty"`
}

func (m *Mapping) normalize() error {
	for _, p := range []*string{&m.Input, &m.Output, &m.System, &m.Split} {
		*p = strings.TrimSpace(*p)
		if len(*p) > 100 {
			return fmt.Errorf("%w: a column name is too long", ErrInvalid)
		}
	}
	m.SystemText = strings.TrimSpace(m.SystemText)
	if len(m.SystemText) > 8000 {
		return fmt.Errorf("%w: the system prompt is too long (8000 characters at most)", ErrInvalid)
	}
	if m.ChunkChars < 0 {
		return fmt.Errorf("%w: chunk_chars must be positive", ErrInvalid)
	}
	if m.ChunkChars != 0 && (m.ChunkChars < minChunk || m.ChunkChars > maxChunk) {
		return fmt.Errorf("%w: chunk_chars is %d to %d", ErrInvalid, minChunk, maxChunk)
	}
	return nil
}

// FileReport is what the build did with one file.
type FileReport struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"` // chat, records, document
	Rows     int    `json:"rows"`
	Examples int    `json:"examples"`
	Skipped  int    `json:"skipped"`
}

// Report is what a build found. It is stored on the dataset row.
type Report struct {
	Files        []FileReport   `json:"files"`
	FilesOmitted int            `json:"files_omitted,omitempty"`
	SkippedTypes map[string]int `json:"skipped_types,omitempty"`
	// Columns are the columns or fields of the first records file, so the
	// mapping editor can offer them.
	Columns   []string `json:"columns,omitempty"`
	Mapping   Mapping  `json:"mapping"`
	Warnings  []string `json:"warnings,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
	Train     int      `json:"train"`
	Eval      int      `json:"eval"`
}

func (r *Report) warn(format string, a ...any) {
	if len(r.Warnings) < maxWarnings {
		r.Warnings = append(r.Warnings, fmt.Sprintf(format, a...))
	}
}

// Archive formats and file kinds, by name.
func archiveKind(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.HasSuffix(n, ".zip"):
		return "zip"
	case strings.HasSuffix(n, ".tar.gz"), strings.HasSuffix(n, ".tgz"):
		return "tgz"
	case strings.HasSuffix(n, ".tar"):
		return "tar"
	}
	return ""
}

func fileKind(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".jsonl", ".ndjson":
		return "jsonl"
	case ".csv":
		return "csv"
	case ".tsv":
		return "tsv"
	case ".txt", ".md", ".markdown":
		return "text"
	}
	return ""
}

// UploadSpec creates an uploaded dataset.
type UploadSpec struct {
	Name       string
	TaskClass  string
	SourceName string // the file's name; its extension says how to read it
	Mapping    Mapping
	HoldoutPct *int // nil takes the default (10); 0 holds nothing out
}

// UploadMax is the largest upload accepted.
func (s *Service) UploadMax() int64 {
	if s.UploadMaxBytes > 0 {
		return s.UploadMaxBytes
	}
	return DefaultUploadMax
}

// CreateUploaded streams body into the blob store, records the dataset and
// queues its build.
func (s *Service) CreateUploaded(ctx context.Context, ownerID uuid.UUID, spec UploadSpec, body io.Reader) (*store.Dataset, error) {
	spec.Name = strings.TrimSpace(spec.Name)
	spec.SourceName = path.Base(strings.ReplaceAll(strings.TrimSpace(spec.SourceName), `\`, "/"))
	if spec.Name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalid)
	}
	if len(spec.Name) > 120 {
		return nil, fmt.Errorf("%w: name is too long", ErrInvalid)
	}
	if archiveKind(spec.SourceName) == "" && fileKind(spec.SourceName) == "" {
		return nil, fmt.Errorf("%w: %q: upload a .jsonl, .csv, .tsv, .txt or .md file, or a .zip or .tar.gz of them", ErrInvalid, spec.SourceName)
	}
	if err := spec.Mapping.normalize(); err != nil {
		return nil, err
	}
	holdout := 10
	if spec.HoldoutPct != nil {
		holdout = *spec.HoldoutPct
	}
	if holdout < 0 || holdout > 50 {
		return nil, fmt.Errorf("%w: holdout_pct is 0 to 50", ErrInvalid)
	}
	limit := s.UploadMax()
	key, n, err := s.Blobs.Put(ctx, io.LimitReader(body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("training: store upload: %w", err)
	}
	if n > limit {
		_ = s.Blobs.Delete(ctx, key)
		return nil, fmt.Errorf("%w: the upload is over the limit of %d bytes (WS_TRAINING_UPLOAD_MAX_BYTES)", ErrInvalid, limit)
	}
	if n == 0 {
		return nil, fmt.Errorf("%w: the upload is empty", ErrInvalid)
	}
	f := Filters{HoldoutPct: holdout, MaxExamples: uploadMaxExamples}
	f.defaults()
	filters, _ := json.Marshal(f)
	mapping, _ := json.Marshal(spec.Mapping)
	src := spec.SourceName
	ds, err := s.DB.CreateUploadedDataset(ctx, store.CreateUploadedDatasetParams{
		OwnerID: ownerID, Name: spec.Name, TaskClass: spec.TaskClass, Filters: filters,
		Mapping: mapping, SourceBlobKey: &key, SourceName: &src, Bytes: n,
	})
	if err != nil {
		return nil, err
	}
	if err := s.enqueueBuild(ctx, ds.ID); err != nil {
		return nil, err
	}
	return &ds, nil
}

func (s *Service) enqueueBuild(ctx context.Context, id uuid.UUID) error {
	if s.EnqueueBuild == nil {
		return nil
	}
	if err := s.EnqueueBuild(ctx, id); err != nil {
		msg := "could not enqueue: " + err.Error()
		_ = s.DB.SetDatasetStatus(ctx, store.SetDatasetStatusParams{ID: id, Status: StatusFailed, Error: &msg})
		return fmt.Errorf("training: enqueue: %w", err)
	}
	return nil
}

// SetMapping changes an uploaded dataset's mapping and queues a rebuild.
func (s *Service) SetMapping(ctx context.Context, id uuid.UUID, m Mapping) (*store.Dataset, error) {
	if err := m.normalize(); err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(m)
	ds, err := s.DB.RequeueDataset(ctx, store.RequeueDatasetParams{ID: id, Mapping: raw})
	if err != nil {
		if _, gerr := s.DB.GetDataset(ctx, id); gerr != nil {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("%w: only an uploaded dataset whose build has finished can be re-mapped", ErrInvalid)
	}
	if err := s.enqueueBuild(ctx, ds.ID); err != nil {
		return nil, err
	}
	return &ds, nil
}

// buildOut collects the two output files as they are written.
type buildOut struct {
	train, eval         bytes.Buffer
	nTrain, nEval       int
	max                 int
	holdout             int
	enc                 *json.Encoder
	scratch             bytes.Buffer
	truncated           bool
	evalByField, byHash int
}

func newBuildOut(f Filters) *buildOut {
	o := &buildOut{max: f.MaxExamples, holdout: f.HoldoutPct}
	o.enc = json.NewEncoder(&o.scratch)
	o.enc.SetEscapeHTML(false)
	return o
}

// add writes one example to the side the split says. side is "train",
// "eval" or "" (decide by the hash of key). It reports false once the
// example cap is reached.
func (o *buildOut) add(ex Example, side, key string) bool {
	if o.nTrain+o.nEval >= o.max {
		o.truncated = true
		return false
	}
	o.scratch.Reset()
	_ = o.enc.Encode(ex)
	switch side {
	case "train":
		o.train.Write(o.scratch.Bytes())
		o.nTrain++
	case "eval":
		o.eval.Write(o.scratch.Bytes())
		o.nEval++
		o.evalByField++
	default:
		o.byHash++
		if HoldoutKey(key, o.holdout) {
			o.eval.Write(o.scratch.Bytes())
			o.nEval++
		} else {
			o.train.Write(o.scratch.Bytes())
			o.nTrain++
		}
	}
	return true
}

// HoldoutKey is Holdout for a string key: a stable hash decides, so the
// same record lands on the same side whenever the dataset is rebuilt.
func HoldoutKey(key string, pct int) bool {
	if pct <= 0 {
		return false
	}
	h := fnv.New32a()
	h.Write([]byte(key))
	return int(h.Sum32()%100) < pct
}

func splitSide(v string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "train", "training":
		return "train", true
	case "eval", "test", "val", "valid", "validation", "dev", "holdout", "held-out":
		return "eval", true
	}
	return "", false
}

// mapUploaded reads a dataset's stored upload through a mapping into train
// and held-out JSONL, held in memory in the returned builder.
func (s *Service) mapUploaded(ctx context.Context, ds store.Dataset, m Mapping, f Filters) (*uploadBuilder, error) {
	if ds.SourceBlobKey == nil || ds.SourceName == nil {
		return nil, errors.New("the dataset has no uploaded file")
	}
	b := &uploadBuilder{m: m, eff: m, cur: m, out: newBuildOut(f), rep: &Report{}}
	err := s.readUpload(ctx, *ds.SourceBlobKey, *ds.SourceName, b.visit)
	b.rep.Mapping = b.eff
	b.rep.Truncated = b.out.truncated
	b.rep.Train, b.rep.Eval = b.out.nTrain, b.out.nEval
	if b.out.truncated {
		b.rep.warn("stopped at %d examples (the cap for one dataset); the rest of the upload was not read", f.MaxExamples)
	}
	return b, err
}

// buildUploaded is Build for an uploaded dataset.
func (s *Service) buildUploaded(ctx context.Context, ds store.Dataset, fail func(error) error) error {
	var f Filters
	_ = json.Unmarshal(ds.Filters, &f)
	if f.MaxExamples <= 0 {
		f.MaxExamples = uploadMaxExamples
	}
	f.defaults()
	var m Mapping
	_ = json.Unmarshal(ds.Mapping, &m)
	b, err := s.mapUploaded(ctx, ds, m, f)
	if b == nil {
		return fail(err)
	}
	out, rep := b.out, b.rep
	if raw, merr := json.Marshal(rep); merr == nil {
		_ = s.DB.SetDatasetReport(ctx, store.SetDatasetReportParams{ID: ds.ID, Report: raw})
	}
	if err != nil {
		return fail(err)
	}
	if out.nTrain == 0 {
		return fail(b.noExamples())
	}
	key, err := blob.PutBytes(ctx, s.Blobs, out.train.Bytes())
	if err != nil {
		return fail(err)
	}
	var evalKey *string
	if out.nEval > 0 {
		k, err := blob.PutBytes(ctx, s.Blobs, out.eval.Bytes())
		if err != nil {
			return fail(err)
		}
		evalKey = &k
	}
	if err := s.DB.FinishDataset(ctx, store.FinishDatasetParams{ID: ds.ID, BlobKey: &key, EvalBlobKey: evalKey, Bytes: int64(out.train.Len()), Examples: int32(out.nTrain), EvalExamples: int32(out.nEval)}); err != nil {
		return fail(err)
	}
	s.log().Info("training: uploaded dataset built", "dataset", ds.ID, "examples", out.nTrain, "eval", out.nEval, "files", len(rep.Files))
	return nil
}

// readUpload opens the stored upload and calls visit for each file in it.
func (s *Service) readUpload(ctx context.Context, key, name string, visit func(name string, r io.Reader) error) error {
	rc, err := s.Blobs.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("read the upload: %w", err)
	}
	defer rc.Close()
	budget := s.UploadMax() * uploadExpand
	limited := func(r io.Reader) io.Reader { return &budgetReader{r: r, left: &budget} }
	switch archiveKind(name) {
	case "zip":
		// zip needs random access: spool to a temp file.
		tmp, err := os.CreateTemp("", "ws-upload-*.zip")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		defer tmp.Close()
		n, err := io.Copy(tmp, rc)
		if err != nil {
			return err
		}
		zr, err := zip.NewReader(tmp, n)
		if err != nil {
			return fmt.Errorf("not a readable zip: %w", err)
		}
		for _, zf := range zr.File {
			if zf.FileInfo().IsDir() {
				continue
			}
			r, err := zf.Open()
			if err != nil {
				return fmt.Errorf("%s: %w", zf.Name, err)
			}
			err = visit(zf.Name, limited(r))
			r.Close()
			if err != nil {
				return err
			}
		}
		return nil
	case "tgz", "tar":
		var src io.Reader = rc
		if archiveKind(name) == "tgz" {
			gz, err := gzip.NewReader(rc)
			if err != nil {
				return fmt.Errorf("not a readable gzip: %w", err)
			}
			defer gz.Close()
			src = gz
		}
		tr := tar.NewReader(src)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return fmt.Errorf("not a readable tar: %w", err)
			}
			if h.Typeflag != tar.TypeReg {
				continue
			}
			if err := visit(h.Name, limited(tr)); err != nil {
				return err
			}
		}
	}
	return visit(name, limited(rc))
}

var errExpandLimit = errors.New("the archive unpacks to more than the upload limit allows (a limit of 8 times WS_TRAINING_UPLOAD_MAX_BYTES)")

type budgetReader struct {
	r    io.Reader
	left *int64
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if *b.left <= 0 {
		return 0, errExpandLimit
	}
	if int64(len(p)) > *b.left {
		p = p[:*b.left]
	}
	n, err := b.r.Read(p)
	*b.left -= int64(n)
	return n, err
}

// uploadBuilder turns the files of an upload into examples.
type uploadBuilder struct {
	m    Mapping
	eff  Mapping // the mapping as applied to the first records file, detected names filled in (for the report)
	cur  Mapping // the mapping for the file being read: m with detected names filled in
	out  *buildOut
	rep  *Report
	read int // files read
}

func (b *uploadBuilder) noExamples() error {
	if len(b.rep.Columns) > 0 && (b.eff.Input == "" || b.eff.Output == "") {
		return fmt.Errorf("no examples: could not tell which columns are the input and the output (found: %s); set the mapping", strings.Join(b.rep.Columns, ", "))
	}
	if b.read == 0 {
		return errors.New("no examples: the upload holds no .jsonl, .csv, .tsv, .txt or .md file")
	}
	return errors.New("no examples: no record had both an input and an output")
}

func (b *uploadBuilder) visit(name string, r io.Reader) error {
	base := path.Base(name)
	if strings.HasPrefix(base, ".") || strings.HasPrefix(name, "__MACOSX/") {
		return nil
	}
	kind := fileKind(base)
	if kind == "" {
		if b.rep.SkippedTypes == nil {
			b.rep.SkippedTypes = map[string]int{}
		}
		ext := strings.ToLower(path.Ext(base))
		if ext == "" {
			ext = "(none)"
		}
		b.rep.SkippedTypes[ext]++
		io.Copy(io.Discard, r)
		return nil
	}
	b.read++
	fr := FileReport{Name: name}
	var err error
	switch kind {
	case "jsonl":
		err = b.jsonl(name, r, &fr)
	case "csv", "tsv":
		err = b.table(name, kind, r, &fr)
	case "text":
		err = b.text(name, r, &fr)
	}
	if len(b.rep.Files) < maxReportFiles {
		b.rep.Files = append(b.rep.Files, fr)
	} else {
		b.rep.FilesOmitted++
	}
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// ---- records ----

var inputNames = []string{"input", "prompt", "question", "instruction", "query", "user", "source"}
var outputNames = []string{"output", "completion", "response", "answer", "reply", "assistant", "target"}

func detect(cols []string, want []string) string {
	for _, w := range want {
		for _, c := range cols {
			if strings.EqualFold(strings.TrimSpace(c), w) {
				return c
			}
		}
	}
	return ""
}

// resolve sets the mapping for a records file: the owner's names, with the
// input and output detected from the file's columns where left empty.
func (b *uploadBuilder) resolve(cols []string) {
	if len(b.rep.Columns) == 0 {
		b.rep.Columns = cols
	}
	b.cur = b.m
	if b.cur.Input == "" {
		b.cur.Input = detect(cols, inputNames)
	}
	if b.cur.Output == "" {
		b.cur.Output = detect(cols, outputNames)
	}
	if b.eff.Input == "" || b.eff.Output == "" {
		b.eff = b.cur
	}
}

func (b *uploadBuilder) record(name string, row int, get func(col string) (string, bool), fr *FileReport) {
	fr.Rows++
	in, _ := get(b.cur.Input)
	outp, _ := get(b.cur.Output)
	in, outp = strings.TrimSpace(in), strings.TrimSpace(outp)
	if in == "" || outp == "" {
		fr.Skipped++
		if fr.Skipped == 1 {
			b.rep.warn("%s row %d: the input or the output is empty (skipped; further rows in this file are counted only)", name, row)
		}
		return
	}
	if len(in)+len(outp) > maxExampleBytes {
		fr.Skipped++
		b.rep.warn("%s row %d: longer than %d bytes (skipped)", name, row, maxExampleBytes)
		return
	}
	var msgs []ChatMessage
	sys := b.cur.SystemText
	if b.cur.System != "" {
		if v, ok := get(b.cur.System); ok && strings.TrimSpace(v) != "" {
			sys = strings.TrimSpace(v)
		}
	}
	if sys != "" {
		msgs = append(msgs, ChatMessage{Role: "system", Content: sys})
	}
	msgs = append(msgs, ChatMessage{Role: "user", Content: in}, ChatMessage{Role: "assistant", Content: outp})
	side := ""
	if b.cur.Split != "" {
		if v, ok := get(b.cur.Split); ok {
			if sd, ok := splitSide(v); ok {
				side = sd
			}
		}
	}
	// Hashing the input keeps a repeated prompt on one side.
	if b.out.add(Example{Messages: msgs, Meta: ExampleMeta{Source: fmt.Sprintf("%s:%d", name, row)}}, side, in) {
		fr.Examples++
	}
}

func (b *uploadBuilder) table(name, kind string, r io.Reader, fr *FileReport) error {
	fr.Kind = "records"
	cr := csv.NewReader(bufio.NewReaderSize(r, 1<<20))
	cr.LazyQuotes = true
	cr.FieldsPerRecord = -1
	if kind == "tsv" {
		cr.Comma = '\t'
	}
	head, err := cr.Read()
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return fmt.Errorf("header: %w", err)
	}
	head[0] = strings.TrimPrefix(head[0], "\ufeff")
	b.resolve(head)
	idx := map[string]int{}
	for i, c := range head {
		if _, dup := idx[c]; !dup {
			idx[c] = i
		}
	}
	if b.cur.Input == "" || b.cur.Output == "" {
		return nil // noExamples explains it with the columns
	}
	for _, need := range []string{b.cur.Input, b.cur.Output} {
		if _, ok := idx[need]; !ok {
			return fmt.Errorf("no column named %q (columns: %s)", need, strings.Join(head, ", "))
		}
	}
	for row := 2; ; row++ {
		rec, err := cr.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			var pe *csv.ParseError
			if !errors.As(err, &pe) {
				return err // a read failure, not a bad row
			}
			fr.Rows++
			fr.Skipped++
			b.rep.warn("%s row %d: %v (skipped)", name, row, err)
			continue
		}
		b.record(name, row, func(col string) (string, bool) {
			i, ok := idx[col]
			if !ok || i >= len(rec) {
				return "", false
			}
			return rec[i], true
		}, fr)
		if b.out.truncated {
			return nil
		}
	}
}

// jsonPath reads a dotted path out of a decoded JSON object.
func jsonPath(obj map[string]any, p string) (string, bool) {
	var cur any = obj
	for _, seg := range strings.Split(p, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		cur, ok = m[seg]
		if !ok {
			return "", false
		}
	}
	switch v := cur.(type) {
	case nil:
		return "", false
	case string:
		return v, true
	default:
		raw, _ := json.Marshal(v)
		return string(raw), true
	}
}

func (b *uploadBuilder) jsonl(name string, r io.Reader, fr *FileReport) error {
	dec := json.NewDecoder(bufio.NewReaderSize(r, 1<<20))
	first := true
	for row := 1; ; row++ {
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("line %d: %w", row, err)
		}
		if _, isChat := obj["messages"].([]any); isChat {
			fr.Kind = "chat"
			fr.Rows++
			b.chat(name, row, obj, fr)
			if b.out.truncated {
				return nil
			}
			continue
		}
		fr.Kind = "records"
		if first {
			first = false
			cols := make([]string, 0, len(obj))
			for k := range obj {
				cols = append(cols, k)
			}
			sort.Strings(cols)
			b.resolve(cols)
			if b.cur.Input == "" || b.cur.Output == "" {
				return nil
			}
		}
		b.record(name, row, func(col string) (string, bool) { return jsonPath(obj, col) }, fr)
		if b.out.truncated {
			return nil
		}
	}
}

// chat takes an already-chat-format line as is: it needs at least one user
// message and a final assistant message.
func (b *uploadBuilder) chat(name string, row int, obj map[string]any, fr *FileReport) {
	raw, _ := json.Marshal(obj["messages"])
	var msgs []ChatMessage
	if err := json.Unmarshal(raw, &msgs); err != nil || len(msgs) < 2 || msgs[len(msgs)-1].Role != "assistant" || msgs[len(msgs)-1].Content == "" {
		fr.Skipped++
		if fr.Skipped == 1 {
			b.rep.warn("%s line %d: messages must end with an assistant message (skipped; further lines in this file are counted only)", name, row)
		}
		return
	}
	key, side := "", ""
	for _, mm := range msgs {
		if mm.Role == "user" {
			key = mm.Content
			break
		}
	}
	if b.m.Split != "" {
		if v, ok := jsonPath(obj, b.m.Split); ok {
			side, _ = splitSide(v)
		}
	}
	if b.out.add(Example{Messages: msgs, Meta: ExampleMeta{Source: fmt.Sprintf("%s:%d", name, row)}}, side, key) {
		fr.Examples++
	}
}

// ---- documents ----

func (b *uploadBuilder) text(name string, r io.Reader, fr *FileReport) error {
	fr.Kind = "document"
	raw, err := io.ReadAll(io.LimitReader(r, 256<<20))
	if err != nil {
		return err
	}
	if !utf8.Valid(raw) {
		raw = bytes.ToValidUTF8(raw, []byte("�"))
	}
	size := b.m.ChunkChars
	if size == 0 {
		size = defaultChunk
	}
	if b.eff.ChunkChars == 0 {
		b.eff.ChunkChars = size
	}
	title := strings.TrimSuffix(path.Base(name), path.Ext(name))
	chunks := Chunk(string(raw), size)
	prev := ""
	for i, c := range chunks {
		fr.Rows++
		prompt := fmt.Sprintf("Write the document %q.", title)
		if prev != "" {
			prompt = fmt.Sprintf("Continue the document %q. It so far ends:\n\n%s", title, tailText(prev, contextTail))
		}
		var msgs []ChatMessage
		if sys := b.m.SystemText; sys != "" {
			msgs = append(msgs, ChatMessage{Role: "system", Content: sys})
		}
		msgs = append(msgs, ChatMessage{Role: "user", Content: prompt}, ChatMessage{Role: "assistant", Content: c})
		if !b.out.add(Example{Messages: msgs, Meta: ExampleMeta{Source: fmt.Sprintf("%s#%d", name, i+1)}}, "", fmt.Sprintf("%s#%d", name, i)) {
			return nil
		}
		fr.Examples++
		prev = c
	}
	return nil
}

// tailText is the last n bytes of s, starting on a rune boundary and, when it
// can, a line start.
func tailText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	t := s[len(s)-n:]
	for len(t) > 0 && !utf8.RuneStart(t[0]) {
		t = t[1:]
	}
	if i := strings.IndexByte(t, '\n'); i >= 0 && i < len(t)-1 {
		t = t[i+1:]
	}
	return t
}

// Chunk cuts text into pieces of about size bytes on paragraph
// boundaries (blank lines); a paragraph longer than size is cut on lines,
// then on spaces, then anywhere. Pieces are trimmed and never empty.
func Chunk(text string, size int) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	for _, para := range strings.Split(text, "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		for _, piece := range splitLong(para, size) {
			if cur.Len() > 0 && cur.Len()+len(piece)+2 > size {
				flush()
			}
			if cur.Len() > 0 {
				cur.WriteString("\n\n")
			}
			cur.WriteString(piece)
		}
	}
	flush()
	return out
}

func splitLong(p string, size int) []string {
	if len(p) <= size {
		return []string{p}
	}
	var out []string
	for len(p) > size {
		cut := size
		if i := strings.LastIndexByte(p[:cut], '\n'); i > size/2 {
			cut = i
		} else if i := strings.LastIndexByte(p[:cut], ' '); i > size/2 {
			cut = i
		}
		for cut > 0 && !utf8.RuneStart(p[cut]) {
			cut--
		}
		out = append(out, strings.TrimSpace(p[:cut]))
		p = strings.TrimSpace(p[cut:])
	}
	if p != "" {
		out = append(out, p)
	}
	return out
}
