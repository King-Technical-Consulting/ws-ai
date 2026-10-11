package docread

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// makePDF builds a small valid PDF with one text line per page.
func makePDF(lines ...string) []byte {
	var b bytes.Buffer
	var offs []int
	obj := func(s string) {
		offs = append(offs, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offs), s)
	}
	b.WriteString("%PDF-1.4\n")
	n := len(lines)
	kids := make([]string, n)
	for i := range lines {
		kids[i] = fmt.Sprintf("%d 0 R", 4+2*i)
	}
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	for i, l := range lines {
		content := fmt.Sprintf("BT /F1 18 Tf 72 700 Td (%s) Tj ET", l)
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 3 0 R >> >> >>", 5+2*i))
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content))
	}
	x := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offs)+1, x)
	return b.Bytes()
}

type stubOCR struct {
	pages []Page
	err   error
	calls int
}

func (s *stubOCR) Recognize(_ context.Context, _, _ string, _ []byte, _ int) ([]Page, error) {
	s.calls++
	return s.pages, s.err
}

func TestDocx(t *testing.T) {
	data := makeZip(t, map[string]string{"word/document.xml": `<w:document xmlns:w="x"><w:body>
<w:p><w:r><w:t>First paragraph</w:t></w:r></w:p>
<w:p><w:r><w:t>Second </w:t></w:r><w:r><w:t>paragraph</w:t></w:r></w:p></w:body></w:document>`})
	r := &Reader{}
	doc, err := r.Extract(context.Background(), "", "notes.docx", data)
	if err != nil {
		t.Fatal(err)
	}
	got := doc.Text("page")
	if !strings.Contains(got, "[page 1]") || !strings.Contains(got, "First paragraph") || !strings.Contains(got, "Second paragraph") {
		t.Errorf("text = %q", got)
	}
	if doc.Method != MethodText {
		t.Errorf("method = %s", doc.Method)
	}
}

func TestXlsxKeepsColumnsAndSheetNames(t *testing.T) {
	data := makeZip(t, map[string]string{
		"xl/workbook.xml":      `<workbook><sheets><sheet name="Budget" sheetId="1"/><sheet name="Notes" sheetId="2"/></sheets></workbook>`,
		"xl/sharedStrings.xml": `<sst><si><t>Item</t></si><si><t>Cost</t></si><si><t>Coffee</t></si><si><r><t>Hello </t></r><r><t>there</t></r></si></sst>`,
		"xl/worksheets/sheet1.xml": `<worksheet><sheetData>
<row r="1"><c r="A1" t="s"><v>0</v></c><c r="C1" t="s"><v>1</v></c></row>
<row r="2"><c r="A2" t="s"><v>2</v></c><c r="C2"><v>4.5</v></c></row></sheetData></worksheet>`,
		"xl/worksheets/sheet2.xml": `<worksheet><sheetData><row r="1"><c r="A1" t="s"><v>3</v></c><c r="B1" t="inlineStr"><is><t>inline</t></is></c></row></sheetData></worksheet>`,
	})
	doc, err := (&Reader{}).Extract(context.Background(), "", "b.xlsx", data)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Pages) != 2 {
		t.Fatalf("pages = %d", len(doc.Pages))
	}
	p1 := doc.Pages[0].Text
	if !strings.HasPrefix(p1, "Budget\n") || !strings.Contains(p1, "Item\t\tCost") || !strings.Contains(p1, "Coffee\t\t4.5") {
		t.Errorf("sheet 1 = %q", p1)
	}
	if p2 := doc.Pages[1].Text; !strings.Contains(p2, "Hello there\tinline") {
		t.Errorf("sheet 2 = %q", p2)
	}
	if !strings.Contains(doc.Text(Unit("xlsx")), "[sheet 2]") {
		t.Error("sheet markers missing")
	}
}

func TestPptxOrdersSlides(t *testing.T) {
	data := makeZip(t, map[string]string{
		"ppt/slides/slide10.xml": `<p:sld><a:p><a:r><a:t>Ten</a:t></a:r></a:p></p:sld>`,
		"ppt/slides/slide2.xml":  `<p:sld><a:p><a:r><a:t>Two</a:t></a:r></a:p></p:sld>`,
		"ppt/slides/slide1.xml":  `<p:sld><a:p><a:r><a:t>One</a:t></a:r></a:p></p:sld>`,
	})
	doc, err := (&Reader{}).Extract(context.Background(), "", "d.pptx", data)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, p := range doc.Pages {
		order = append(order, p.Text)
	}
	if strings.Join(order, ",") != "One,Two,Ten" {
		t.Errorf("slides = %v", order)
	}
}

func TestPDFTextByPage(t *testing.T) {
	text := "This is the first page of a normal document with plenty of text."
	data := makePDF(text, "Second page has more words in it for the layer.")
	doc, err := (&Reader{}).Extract(context.Background(), "application/pdf", "a.pdf", data)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Pages) != 2 || !strings.Contains(doc.Pages[0].Text, "first page") || !strings.Contains(doc.Pages[1].Text, "Second page") {
		t.Errorf("pages = %+v", doc.Pages)
	}
	if doc.Method != MethodText {
		t.Errorf("method = %s", doc.Method)
	}
}

func TestScannedPDFGoesToOCR(t *testing.T) {
	data := makePDF("x") // a text layer too thin to be the document
	stub := &stubOCR{pages: []Page{{N: 1, Text: "Recognised text"}}}
	doc, err := (&Reader{OCR: stub}).Extract(context.Background(), "application/pdf", "scan.pdf", data)
	if err != nil {
		t.Fatal(err)
	}
	if stub.calls != 1 || doc.Method != MethodOCR || doc.Pages[0].Text != "Recognised text" {
		t.Errorf("calls=%d doc=%+v", stub.calls, doc)
	}
	// Without OCR the scanned file says what it needs instead of returning nothing.
	if _, err := (&Reader{}).Extract(context.Background(), "application/pdf", "scan.pdf", makePDF("")); !errors.Is(err, ErrNeedsOCR) {
		t.Errorf("no OCR: %v", err)
	}
}

func TestTextPDFDoesNotCallOCR(t *testing.T) {
	stub := &stubOCR{}
	data := makePDF(strings.Repeat("A real text layer. ", 5))
	if _, err := (&Reader{OCR: stub}).Extract(context.Background(), "application/pdf", "a.pdf", data); err != nil {
		t.Fatal(err)
	}
	if stub.calls != 0 {
		t.Error("a document with text must not be sent to OCR")
	}
}

func TestImageNeedsOCR(t *testing.T) {
	if _, err := (&Reader{}).Extract(context.Background(), "image/png", "p.png", []byte("\x89PNG")); !errors.Is(err, ErrNeedsOCR) {
		t.Errorf("image without OCR: %v", err)
	}
	stub := &stubOCR{pages: []Page{{N: 1, Text: "from the picture"}}}
	doc, err := (&Reader{OCR: stub}).Extract(context.Background(), "image/png", "p.png", []byte("\x89PNG"))
	if err != nil || doc.Pages[0].Text != "from the picture" {
		t.Errorf("image: %+v %v", doc, err)
	}
	stub.err = errors.New("boom")
	if _, err := (&Reader{OCR: stub}).Extract(context.Background(), "image/png", "p.png", []byte("x")); err == nil {
		t.Error("ocr failure should surface")
	}
}

func TestLimits(t *testing.T) {
	r := &Reader{Limits: Limits{MaxBytes: 10}}
	if _, err := r.Extract(context.Background(), "text/plain", "a.txt", []byte(strings.Repeat("x", 11))); !errors.Is(err, ErrTooBig) {
		t.Errorf("size: %v", err)
	}
	r = &Reader{Limits: Limits{MaxPages: 2}}
	data := makePDF(strings.Repeat("page one text. ", 3), strings.Repeat("page two text. ", 3), strings.Repeat("page three text. ", 3))
	doc, err := r.Extract(context.Background(), "application/pdf", "a.pdf", data)
	if err != nil || len(doc.Pages) != 2 || !doc.Truncated {
		t.Errorf("page limit: %+v %v", doc, err)
	}
	// A zip part that inflates past the cap is refused.
	big := makeZip(t, map[string]string{"word/document.xml": "<w:t>" + strings.Repeat("a", 5000) + "</w:t>"})
	r = &Reader{Limits: Limits{MaxZipBytes: 1000}}
	if _, err := r.Extract(context.Background(), "", "a.docx", big); !errors.Is(err, ErrTooBig) {
		t.Errorf("zip bomb guard: %v", err)
	}
}

func TestUnsupportedAndBinary(t *testing.T) {
	r := &Reader{}
	if _, err := r.Extract(context.Background(), "application/zip", "a.zip", []byte("PK")); !errors.Is(err, ErrUnsupported) {
		t.Errorf("zip: %v", err)
	}
	if _, err := r.Extract(context.Background(), "text/plain", "a.txt", []byte("ab\x00cd")); !errors.Is(err, ErrUnsupported) {
		t.Errorf("binary text: %v", err)
	}
	if _, err := r.Extract(context.Background(), "application/pdf", "bad.pdf", []byte("not a pdf at all")); err == nil {
		t.Error("garbage pdf should error, not panic")
	}
}

func TestPlainTextPaging(t *testing.T) {
	para := strings.Repeat("word ", 40) + "\n" // 201 bytes
	doc, err := (&Reader{}).Extract(context.Background(), "text/plain", "long.txt", []byte(strings.Repeat(para, 60)))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Pages) < 3 {
		t.Fatalf("pages = %d", len(doc.Pages))
	}
	var rejoined strings.Builder
	for _, p := range doc.Pages {
		rejoined.WriteString(p.Text)
	}
	if rejoined.String() != strings.Repeat(para, 60) {
		t.Error("paging lost or changed text")
	}
}

func TestKind(t *testing.T) {
	cases := map[string]string{
		"a.pdf": "pdf", "A.DOCX": "docx", "x.xlsx": "xlsx", "d.pptx": "pptx", "n.md": "text", "p.png": "", "data.bin": "",
	}
	for name, want := range cases {
		if got := Kind("", name); got != want {
			t.Errorf("Kind(%q) = %q, want %q", name, got, want)
		}
	}
	if Kind("image/jpeg", "x") != "image" || Kind("application/pdf", "x") != "pdf" {
		t.Error("mime classification")
	}
}

func TestHTTPOCRContract(t *testing.T) {
	var gotName, gotMax, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ocr" || r.Method != http.MethodPost {
			http.Error(w, "no", 404)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		f, h, _ := r.FormFile("file")
		defer f.Close()
		gotName, gotMax, gotType = h.Filename, r.FormValue("max_pages"), h.Header.Get("Content-Type")
		_ = json.NewEncoder(w).Encode(map[string]any{"pages": []Page{{Text: "hello"}, {N: 5, Text: "world"}}})
	}))
	defer srv.Close()
	o := &HTTPOCR{URL: srv.URL + "/"}
	pages, err := o.Recognize(context.Background(), "image/png", `we"ird/na\me.png`, []byte("img"), 7)
	if err != nil {
		t.Fatal(err)
	}
	if gotMax != "7" || gotType != "image/png" || strings.ContainsAny(gotName, `"/\`) {
		t.Errorf("sent: name=%q max=%q type=%q", gotName, gotMax, gotType)
	}
	if len(pages) != 2 || pages[0].N != 1 || pages[1].N != 5 {
		t.Errorf("pages = %+v", pages)
	}
	down := &HTTPOCR{URL: "http://127.0.0.1:1"}
	if _, err := down.Recognize(context.Background(), "image/png", "a.png", nil, 1); err == nil || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("unreachable service error should not carry the address: %v", err)
	}
}
