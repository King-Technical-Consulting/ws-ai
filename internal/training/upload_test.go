package training

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for n, c := range files {
		w, _ := zw.Create(n)
		w.Write([]byte(c))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func tgzOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for n, c := range files {
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(c)), Typeflag: tar.TypeReg})
		tw.Write([]byte(c))
	}
	tw.Close()
	gz.Close()
	return b.Bytes()
}

// upload runs the whole path: create, build, and return the dataset, its
// report and its examples.
func upload(t *testing.T, name string, body []byte, m Mapping, holdout int) (*fakeStore, *Service, uuid.UUID, Report) {
	t.Helper()
	db := newFake()
	svc := newService(t, db)
	ctx := context.Background()
	ds, err := svc.CreateUploaded(ctx, uuid.New(), UploadSpec{Name: "up", SourceName: name, Mapping: m, HoldoutPct: &holdout}, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Build(ctx, ds.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetDataset(ctx, ds.ID)
	var rep Report
	_ = json.Unmarshal(got.Report, &rep)
	return db, svc, ds.ID, rep
}

func ptr(n int) *int { return &n }

func csvOf(n int) string {
	var b strings.Builder
	b.WriteString("id,question,answer,notes\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "%d,\"what is %d, exactly?\",\"it is %d\",x\n", i, i, i)
	}
	return b.String()
}

func TestUploadCSVDetectsColumns(t *testing.T) {
	db, svc, id, rep := upload(t, "qa.csv", []byte(csvOf(100)), Mapping{}, 20)
	ds, _ := db.GetDataset(context.Background(), id)
	if ds.Status != StatusReady || ds.Kind != KindUploaded {
		t.Fatalf("dataset = %+v", ds)
	}
	if int(ds.Examples+ds.EvalExamples) != 100 || ds.EvalExamples == 0 {
		t.Errorf("examples %d + eval %d", ds.Examples, ds.EvalExamples)
	}
	if rep.Mapping.Input != "question" || rep.Mapping.Output != "answer" {
		t.Errorf("detected mapping = %+v", rep.Mapping)
	}
	if len(rep.Columns) != 4 {
		t.Errorf("columns = %v", rep.Columns)
	}
	exs, err := svc.Preview(context.Background(), id, false, 3)
	if err != nil || len(exs) != 3 {
		t.Fatalf("preview: %v %d", err, len(exs))
	}
	m := exs[0].Messages
	if len(m) != 2 || m[0].Role != "user" || !strings.HasPrefix(m[0].Content, "what is ") || m[1].Role != "assistant" || exs[0].Meta.Source == "" {
		t.Errorf("example = %+v", exs[0])
	}
}

func TestUploadSplitIsStable(t *testing.T) {
	body := []byte(csvOf(200))
	_, _, _, a := upload(t, "qa.csv", body, Mapping{}, 25)
	_, _, _, b := upload(t, "qa.csv", body, Mapping{}, 25)
	if a.Eval != b.Eval || a.Eval == 0 || a.Train+a.Eval != 200 {
		t.Errorf("eval %d vs %d, train %d", a.Eval, b.Eval, a.Train)
	}
}

func TestUploadSplitColumnAndSystem(t *testing.T) {
	csv := "q,a,sp,sys\nA,1,train,be brief\nB,2,test,\nC,3,TRAIN,\nD,4,other,\n"
	db, svc, id, rep := upload(t, "x.csv", []byte(csv), Mapping{Input: "q", Output: "a", Split: "sp", System: "sys", SystemText: "default"}, 0)
	ds, _ := db.GetDataset(context.Background(), id)
	// A, C pinned train; B pinned eval; D has no valid split value: hashed
	// with a holdout of 0, so it trains.
	if ds.Examples != 3 || ds.EvalExamples != 1 {
		t.Fatalf("train %d eval %d (%+v)", ds.Examples, ds.EvalExamples, rep)
	}
	exs, _ := svc.Preview(context.Background(), id, false, 10)
	var sawBrief, sawDefault bool
	for _, e := range exs {
		if e.Messages[0].Role == "system" {
			switch e.Messages[0].Content {
			case "be brief":
				sawBrief = true
			case "default":
				sawDefault = true
			}
		}
	}
	if !sawBrief || !sawDefault {
		t.Errorf("system prompts: brief=%v default=%v", sawBrief, sawDefault)
	}
}

func TestUploadUnknownColumnsFailWithTheColumnsThenRemap(t *testing.T) {
	csv := "alpha,beta\none,two\nthree,four\n"
	db, svc, id, rep := upload(t, "x.csv", []byte(csv), Mapping{}, 0)
	ctx := context.Background()
	ds, _ := db.GetDataset(ctx, id)
	if ds.Status != StatusFailed || ds.Error == nil || !strings.Contains(*ds.Error, "alpha, beta") {
		t.Fatalf("dataset = %+v", ds)
	}
	if len(rep.Columns) != 2 {
		t.Errorf("columns = %v", rep.Columns)
	}
	if _, err := svc.SetMapping(ctx, id, Mapping{Input: "alpha", Output: "beta"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Build(ctx, id); err != nil {
		t.Fatal(err)
	}
	ds, _ = db.GetDataset(ctx, id)
	if ds.Status != StatusReady || ds.Examples != 2 {
		t.Errorf("after remap = %+v", ds)
	}
	// a ready dataset can be re-mapped, a chat dataset cannot
	if _, err := svc.SetMapping(ctx, id, Mapping{Input: "beta", Output: "alpha"}); err != nil {
		t.Errorf("remap ready: %v", err)
	}
	if _, err := svc.SetMapping(ctx, uuid.New(), Mapping{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id: %v", err)
	}
	if _, err := svc.SetMapping(ctx, id, Mapping{ChunkChars: 5}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad chunk size: %v", err)
	}
}

func TestUploadMissingNamedColumn(t *testing.T) {
	db, _, id, _ := upload(t, "x.tsv", []byte("a\tb\n1\t2\n"), Mapping{Input: "nope", Output: "b"}, 0)
	ds, _ := db.GetDataset(context.Background(), id)
	if ds.Status != StatusFailed || !strings.Contains(*ds.Error, `no column named "nope"`) {
		t.Errorf("dataset = %+v err=%v", ds.Status, ds.Error)
	}
}

func TestUploadJSONLRecordsAndChatAndPaths(t *testing.T) {
	jl := `{"prompt":"p1","completion":"c1"}
{"prompt":"p2","completion":"c2"}
{"prompt":"","completion":"c3"}
{"prompt":"p4","completion":"c4"}
`
	db, _, id, rep := upload(t, "r.jsonl", []byte(jl), Mapping{}, 0)
	ds, _ := db.GetDataset(context.Background(), id)
	if ds.Examples != 3 || rep.Files[0].Skipped != 1 || rep.Files[0].Kind != "records" {
		t.Errorf("records: %+v %+v", ds, rep.Files)
	}
	chat := `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"}]}
{"messages":[{"role":"user","content":"hi"}]}
`
	db, _, id, rep = upload(t, "c.jsonl", []byte(chat), Mapping{}, 0)
	ds, _ = db.GetDataset(context.Background(), id)
	if ds.Examples != 1 || rep.Files[0].Kind != "chat" || rep.Files[0].Skipped != 1 {
		t.Errorf("chat: %+v %+v", ds, rep.Files)
	}
	nested := `{"turn":{"q":"deep","a":{"text":"answer"}}}` + "\n"
	db, svc, id, _ := upload(t, "n.jsonl", []byte(nested), Mapping{Input: "turn.q", Output: "turn.a.text"}, 0)
	ds, _ = db.GetDataset(context.Background(), id)
	exs, _ := svc.Preview(context.Background(), id, false, 1)
	if ds.Examples != 1 || exs[0].Messages[1].Content != "answer" {
		t.Errorf("dotted path: %+v", exs)
	}
}

func TestUploadDocumentChunksContinueEachOther(t *testing.T) {
	var doc strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&doc, "Paragraph %d. %s\n\n", i, strings.Repeat("word ", 40))
	}
	db, svc, id, rep := upload(t, "essay.md", []byte(doc.String()), Mapping{ChunkChars: 1000}, 0)
	ds, _ := db.GetDataset(context.Background(), id)
	if ds.Examples < 5 || rep.Files[0].Kind != "document" {
		t.Fatalf("dataset = %+v files=%+v", ds, rep.Files)
	}
	exs, _ := svc.Preview(context.Background(), id, false, 3)
	if !strings.Contains(exs[0].Messages[0].Content, "Write the document \"essay\"") {
		t.Errorf("first prompt = %q", exs[0].Messages[0].Content)
	}
	if !strings.Contains(exs[1].Messages[0].Content, "Continue the document") || !strings.Contains(exs[1].Messages[0].Content, "word") {
		t.Errorf("second prompt = %q", exs[1].Messages[0].Content)
	}
	for _, e := range exs {
		if len(e.Messages[1].Content) > 1100 {
			t.Errorf("chunk of %d bytes", len(e.Messages[1].Content))
		}
	}
}

func TestUploadArchives(t *testing.T) {
	files := map[string]string{
		"data/a.csv":      csvOf(10),
		"data/b.jsonl":    `{"input":"i","output":"o"}` + "\n",
		"notes/n.txt":     "A short note.",
		"img/spec.png":    "not text",
		"__MACOSX/._a":    "junk",
		"data/.DS_Store":  "junk",
		"data/readme.pdf": "skip me",
	}
	for name, body := range map[string][]byte{"set.zip": zipOf(t, files), "set.tar.gz": tgzOf(t, files)} {
		db, _, id, rep := upload(t, name, body, Mapping{}, 0)
		ds, _ := db.GetDataset(context.Background(), id)
		if ds.Status != StatusReady || ds.Examples != 12 {
			t.Errorf("%s: status %s examples %d err=%v", name, ds.Status, ds.Examples, ds.Error)
		}
		if len(rep.Files) != 3 || rep.SkippedTypes[".png"] != 1 || rep.SkippedTypes[".pdf"] != 1 {
			t.Errorf("%s: report = %+v", name, rep)
		}
	}
}

func TestUploadRejects(t *testing.T) {
	db := newFake()
	svc := newService(t, db)
	ctx := context.Background()
	owner := uuid.New()
	for _, c := range []struct {
		name  string
		spec  UploadSpec
		body  string
		limit int64
	}{
		{"no name", UploadSpec{SourceName: "a.csv"}, "x", 0},
		{"bad type", UploadSpec{Name: "n", SourceName: "a.exe"}, "x", 0},
		{"empty", UploadSpec{Name: "n", SourceName: "a.csv"}, "", 0},
		{"bad holdout", UploadSpec{Name: "n", SourceName: "a.csv", HoldoutPct: ptr(80)}, "x", 0},
		{"too big", UploadSpec{Name: "n", SourceName: "a.csv"}, "0123456789", 5},
	} {
		svc.UploadMaxBytes = c.limit
		if _, err := svc.CreateUploaded(ctx, owner, c.spec, strings.NewReader(c.body)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v", c.name, err)
		}
	}
	if len(db.datasets) != 0 {
		t.Errorf("a rejected upload left %d datasets", len(db.datasets))
	}
}

func TestUploadExpansionLimit(t *testing.T) {
	db := newFake()
	svc := newService(t, db)
	ctx := context.Background()
	// a highly compressible file that unpacks past 8x the limit
	big := strings.Repeat("a,b\n", 200)
	body := zipOf(t, map[string]string{"x.csv": "q,a\n" + big})
	svc.UploadMaxBytes = int64(len(body)) + 1
	ds, err := svc.CreateUploaded(ctx, uuid.New(), UploadSpec{Name: "n", SourceName: "z.zip", Mapping: Mapping{Input: "q", Output: "a"}}, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	svc.UploadMaxBytes = 10 // shrink the budget after the upload was accepted
	_ = svc.Build(ctx, ds.ID)
	got, _ := db.GetDataset(ctx, ds.ID)
	if got.Status != StatusFailed || !strings.Contains(*got.Error, "unpacks to more") {
		t.Errorf("status %s err %v", got.Status, got.Error)
	}
}

func TestChunk(t *testing.T) {
	if got := Chunk("  \n\n  ", 500); len(got) != 0 {
		t.Errorf("blank: %q", got)
	}
	long := strings.Repeat("é", 700) // no spaces, multibyte
	for _, c := range Chunk(long, 300) {
		if len(c) > 300 || !strings.HasPrefix(c, "é") {
			t.Errorf("chunk of %d bytes", len(c))
		}
	}
	got := Chunk("a\n\nb\n\nc", 1000)
	if len(got) != 1 || got[0] != "a\n\nb\n\nc" {
		t.Errorf("small = %q", got)
	}
}
