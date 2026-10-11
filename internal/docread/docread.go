// Package docread turns an uploaded document into text with page markers
// (PLAN M13 cut 4). PDF text, docx, xlsx, pptx and plain text are read in
// Go; images and PDFs with almost no text layer go to an OCR container
// (OCR_URL), which is a black box that takes an image or a PDF and returns
// text per page. Nothing here executes document content.
package docread

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
)

// Method is how the text was obtained.
const (
	MethodText = "text"
	MethodOCR  = "ocr"
)

// Page is one page (or sheet, or slide) of text.
type Page struct {
	N    int    `json:"n"`
	Text string `json:"text"`
}

// Doc is an extracted document.
type Doc struct {
	Pages  []Page `json:"pages"`
	Method string `json:"method"`
	// Truncated is set when the page limit cut the document short.
	Truncated bool `json:"truncated,omitempty"`
}

// Chars is the total text length.
func (d *Doc) Chars() int {
	n := 0
	for _, p := range d.Pages {
		n += len(strings.TrimSpace(p.Text))
	}
	return n
}

// Text renders the document with page markers: "[page 3]" before each
// page's text, or "[sheet 2]" and "[slide 4]" style via Unit.
func (d *Doc) Text(unit string) string {
	if unit == "" {
		unit = "page"
	}
	var b strings.Builder
	for i, p := range d.Pages {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "[%s %d]\n%s", unit, p.N, strings.TrimSpace(p.Text))
	}
	return b.String()
}

// Errors.
var (
	ErrUnsupported = errors.New("docread: unsupported document type")
	ErrTooBig      = errors.New("docread: document is over the size limit")
	ErrNeedsOCR    = errors.New("docread: the document has no text layer and OCR is not configured")
)

// Limits bounds extraction. Zero values mean the defaults.
type Limits struct {
	MaxBytes int64 // upload size
	MaxPages int   // pages (or sheets, slides) read; the rest are dropped and Truncated is set
	// MaxZipBytes bounds the uncompressed size of one part of an Office file
	// (a zip bomb guard).
	MaxZipBytes int64
}

func (l Limits) withDefaults() Limits {
	if l.MaxBytes <= 0 {
		l.MaxBytes = 25 << 20
	}
	if l.MaxPages <= 0 {
		l.MaxPages = 200
	}
	if l.MaxZipBytes <= 0 {
		l.MaxZipBytes = 64 << 20
	}
	return l
}

// OCR is the black-box recogniser.
type OCR interface {
	// Recognize returns text per page for an image or a PDF.
	Recognize(ctx context.Context, mime, name string, data []byte, maxPages int) ([]Page, error)
}

// Reader extracts documents.
type Reader struct {
	Limits Limits
	OCR    OCR // nil when no OCR_URL is set
}

// Kind classifies a file by its media type and name: "pdf", "docx",
// "xlsx", "pptx", "text", "image", or "" when it is not a document.
func Kind(mime, name string) string {
	m := strings.ToLower(strings.TrimSpace(strings.SplitN(mime, ";", 2)[0]))
	ext := strings.ToLower(path.Ext(name))
	switch {
	case m == "application/pdf" || ext == ".pdf":
		return "pdf"
	case m == "application/vnd.openxmlformats-officedocument.wordprocessingml.document" || ext == ".docx":
		return "docx"
	case m == "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" || ext == ".xlsx":
		return "xlsx"
	case m == "application/vnd.openxmlformats-officedocument.presentationml.presentation" || ext == ".pptx":
		return "pptx"
	case strings.HasPrefix(m, "image/"):
		return "image"
	case strings.HasPrefix(m, "text/"), m == "application/json", m == "application/xml",
		ext == ".txt", ext == ".md", ext == ".csv", ext == ".json", ext == ".log", ext == ".tsv", ext == ".yaml", ext == ".yml":
		return "text"
	}
	return ""
}

// Unit is the marker word for a kind.
func Unit(kind string) string {
	switch kind {
	case "xlsx":
		return "sheet"
	case "pptx":
		return "slide"
	}
	return "page"
}

// Extract reads one document.
func (r *Reader) Extract(ctx context.Context, mime, name string, data []byte) (*Doc, error) {
	lim := r.Limits.withDefaults()
	if int64(len(data)) > lim.MaxBytes {
		return nil, fmt.Errorf("%w (%d MiB; the limit is %d MiB)", ErrTooBig, len(data)>>20, lim.MaxBytes>>20)
	}
	var doc *Doc
	var err error
	switch kind := Kind(mime, name); kind {
	case "pdf":
		doc, err = r.pdf(ctx, mime, name, data, lim)
	case "docx":
		doc, err = docx(data, lim)
	case "xlsx":
		doc, err = xlsx(data, lim)
	case "pptx":
		doc, err = pptx(data, lim)
	case "text":
		doc, err = plain(data, lim)
	case "image":
		doc, err = r.ocr(ctx, mime, name, data, lim)
	default:
		return nil, ErrUnsupported
	}
	if err != nil {
		return nil, err
	}
	if len(doc.Pages) > lim.MaxPages {
		doc.Pages = doc.Pages[:lim.MaxPages]
		doc.Truncated = true
	}
	return doc, nil
}

func (r *Reader) ocr(ctx context.Context, mime, name string, data []byte, lim Limits) (*Doc, error) {
	if r.OCR == nil {
		return nil, ErrNeedsOCR
	}
	pages, err := r.OCR.Recognize(ctx, mime, name, data, lim.MaxPages)
	if err != nil {
		return nil, fmt.Errorf("docread: ocr: %w", err)
	}
	return &Doc{Pages: pages, Method: MethodOCR}, nil
}

// pdf reads the text layer; a document whose layer is nearly empty is
// treated as scanned and sent to OCR when it is configured.
func (r *Reader) pdf(ctx context.Context, mime, name string, data []byte, lim Limits) (doc *Doc, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			doc, err = nil, fmt.Errorf("docread: unreadable pdf: %v", rec)
		}
	}()
	rd, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("docread: unreadable pdf: %w", err)
	}
	n := rd.NumPage()
	doc = &Doc{Method: MethodText}
	for i := 1; i <= n && i <= lim.MaxPages+1; i++ {
		t, perr := rd.Page(i).GetPlainText(nil)
		if perr != nil {
			t = ""
		}
		doc.Pages = append(doc.Pages, Page{N: i, Text: t})
	}
	if n > lim.MaxPages {
		doc.Truncated = true
		doc.Pages = doc.Pages[:lim.MaxPages]
	}
	if scanned(doc, n) {
		if r.OCR == nil {
			if doc.Chars() == 0 {
				return nil, ErrNeedsOCR
			}
			return doc, nil
		}
		return r.ocr(ctx, mime, name, data, lim)
	}
	return doc, nil
}

// scanned reports whether a PDF's text layer is too thin to be the
// document: under 25 characters a page on average over the pages read.
func scanned(d *Doc, pages int) bool {
	if len(d.Pages) == 0 {
		return pages > 0
	}
	return d.Chars() < 25*len(d.Pages)
}

func plain(data []byte, lim Limits) (*Doc, error) {
	if !utf8.Valid(data) {
		// Latin-1 and Windows-1252 text is common in old files; keep the
		// bytes that are text and drop the rest rather than refuse.
		data = bytes.ToValidUTF8(data, []byte("?"))
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return nil, fmt.Errorf("%w: binary content", ErrUnsupported)
	}
	// One "page" per 4,000 characters so a long text file can be paged.
	const per = 4000
	s := string(data)
	var pages []Page
	for i, n := 0, 1; i < len(s); n++ {
		j := i + per
		if j >= len(s) {
			j = len(s)
		} else if k := strings.LastIndex(s[i:j], "\n"); k > per/2 {
			j = i + k + 1
		} else {
			for j < len(s) && !utf8.RuneStart(s[j]) {
				j++
			}
		}
		pages = append(pages, Page{N: n, Text: s[i:j]})
		i = j
	}
	if len(pages) == 0 {
		pages = []Page{{N: 1}}
	}
	return &Doc{Pages: pages, Method: MethodText}, nil
}

// ---- Office Open XML ----

func openZip(data []byte, lim Limits) (*zip.Reader, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("docread: not an Office file: %w", err)
	}
	return zr, nil
}

func readPart(zr *zip.Reader, name string, lim Limits) ([]byte, error) {
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		if int64(f.UncompressedSize64) > lim.MaxZipBytes {
			return nil, fmt.Errorf("%w: %s is %d MiB uncompressed", ErrTooBig, name, f.UncompressedSize64>>20)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, lim.MaxZipBytes))
	}
	return nil, fmt.Errorf("docread: %s not in file", name)
}

// xmlText collects the character data of elements named one of tags, with a
// newline at the end of each element named one of breaks, and a tab at each
// of tabs. It is enough for runs (w:t, a:t) and paragraphs (w:p, a:p).
func xmlText(data []byte, tags, breaks map[string]bool) string {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	var b strings.Builder
	in := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if tags[t.Name.Local] {
				in++
			}
			if t.Name.Local == "tab" {
				b.WriteByte('\t')
			}
			if t.Name.Local == "br" {
				b.WriteByte('\n')
			}
		case xml.EndElement:
			if tags[t.Name.Local] && in > 0 {
				in--
			}
			if breaks[t.Name.Local] {
				b.WriteByte('\n')
			}
		case xml.CharData:
			if in > 0 {
				b.Write(t)
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func docx(data []byte, lim Limits) (*Doc, error) {
	zr, err := openZip(data, lim)
	if err != nil {
		return nil, err
	}
	body, err := readPart(zr, "word/document.xml", lim)
	if err != nil {
		return nil, err
	}
	text := xmlText(body, map[string]bool{"t": true}, map[string]bool{"p": true})
	// A Word file has no stored pages; split on explicit page breaks, else
	// cut into pages of about 3,000 characters on paragraph boundaries.
	return &Doc{Pages: chunkPages(text, 3000), Method: MethodText}, nil
}

func pptx(data []byte, lim Limits) (*Doc, error) {
	zr, err := openZip(data, lim)
	if err != nil {
		return nil, err
	}
	type slide struct {
		n    int
		name string
	}
	re := regexp.MustCompile(`^ppt/slides/slide(\d+)\.xml$`)
	var slides []slide
	for _, f := range zr.File {
		if m := re.FindStringSubmatch(f.Name); m != nil {
			n, _ := strconv.Atoi(m[1])
			slides = append(slides, slide{n, f.Name})
		}
	}
	sort.Slice(slides, func(i, j int) bool { return slides[i].n < slides[j].n })
	doc := &Doc{Method: MethodText}
	for i, s := range slides {
		if i >= lim.MaxPages+1 {
			break
		}
		b, err := readPart(zr, s.name, lim)
		if err != nil {
			return nil, err
		}
		doc.Pages = append(doc.Pages, Page{N: s.n, Text: xmlText(b, map[string]bool{"t": true}, map[string]bool{"p": true})})
	}
	if len(doc.Pages) == 0 {
		return nil, fmt.Errorf("docread: no slides in file")
	}
	return doc, nil
}

func xlsx(data []byte, lim Limits) (*Doc, error) {
	zr, err := openZip(data, lim)
	if err != nil {
		return nil, err
	}
	// Shared strings: the text of every string cell, by index.
	var shared []string
	if b, err := readPart(zr, "xl/sharedStrings.xml", lim); err == nil {
		dec := xml.NewDecoder(bytes.NewReader(b))
		dec.Strict = false
		var cur strings.Builder
		inSI, inT := false, false
		for {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			switch t := tok.(type) {
			case xml.StartElement:
				switch t.Name.Local {
				case "si":
					inSI = true
					cur.Reset()
				case "t":
					inT = inSI
				}
			case xml.EndElement:
				switch t.Name.Local {
				case "si":
					inSI = false
					shared = append(shared, cur.String())
				case "t":
					inT = false
				}
			case xml.CharData:
				if inT {
					cur.Write(t)
				}
			}
		}
	}
	// Sheet names in order, from the workbook.
	type sheet struct {
		name string
		file string
	}
	var sheets []sheet
	re := regexp.MustCompile(`^xl/worksheets/sheet(\d+)\.xml$`)
	var files []string
	for _, f := range zr.File {
		if re.MatchString(f.Name) {
			files = append(files, f.Name)
		}
	}
	sort.Slice(files, func(i, j int) bool {
		a, _ := strconv.Atoi(re.FindStringSubmatch(files[i])[1])
		b, _ := strconv.Atoi(re.FindStringSubmatch(files[j])[1])
		return a < b
	})
	names := workbookSheetNames(zr, lim)
	for i, f := range files {
		name := fmt.Sprintf("Sheet%d", i+1)
		if i < len(names) && names[i] != "" {
			name = names[i]
		}
		sheets = append(sheets, sheet{name, f})
	}
	if len(sheets) == 0 {
		return nil, fmt.Errorf("docread: no sheets in file")
	}
	doc := &Doc{Method: MethodText}
	for i, s := range sheets {
		if i >= lim.MaxPages+1 {
			break
		}
		b, err := readPart(zr, s.file, lim)
		if err != nil {
			return nil, err
		}
		doc.Pages = append(doc.Pages, Page{N: i + 1, Text: s.name + "\n" + sheetRows(b, shared)})
	}
	return doc, nil
}

func workbookSheetNames(zr *zip.Reader, lim Limits) []string {
	b, err := readPart(zr, "xl/workbook.xml", lim)
	if err != nil {
		return nil
	}
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = false
	var out []string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "sheet" {
			for _, a := range se.Attr {
				if a.Name.Local == "name" {
					out = append(out, a.Value)
				}
			}
		}
	}
	return out
}

// sheetRows renders one worksheet as tab-separated rows; empty cells keep
// their column so a row reads as the sheet does.
func sheetRows(data []byte, shared []string) string {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	var rows []string
	var cells []string
	var col int
	var typ string
	var val strings.Builder
	var inV, inIS, inT bool
	var inRow bool
	flushCell := func() {
		v := val.String()
		if typ == "s" {
			if i, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && i >= 0 && i < len(shared) {
				v = shared[i]
			}
		}
		for len(cells) < col {
			cells = append(cells, "")
		}
		cells = append(cells, strings.TrimSpace(v))
		val.Reset()
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				inRow, cells = true, nil
			case "c":
				typ, col = "", len(cells)
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "t":
						typ = a.Value
					case "r":
						col = colIndex(a.Value)
					}
				}
				val.Reset()
			case "v":
				inV = true
			case "is":
				inIS = true
			case "t":
				inT = inIS
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "c":
				flushCell()
			case "v":
				inV = false
			case "is":
				inIS = false
			case "t":
				inT = false
			case "row":
				if inRow {
					rows = append(rows, strings.TrimRight(strings.Join(cells, "\t"), "\t"))
				}
				inRow = false
			}
		case xml.CharData:
			if inV || inT {
				val.Write(t)
			}
		}
	}
	return strings.TrimSpace(strings.Join(rows, "\n"))
}

// colIndex turns a cell reference like "C7" into the zero-based column 2.
func colIndex(ref string) int {
	n := 0
	for _, r := range ref {
		if r < 'A' || r > 'Z' {
			break
		}
		n = n*26 + int(r-'A') + 1
	}
	return n - 1
}

// chunkPages splits text into pages of about size characters on blank-line
// or newline boundaries.
func chunkPages(text string, size int) []Page {
	text = strings.TrimSpace(text)
	if text == "" {
		return []Page{{N: 1}}
	}
	var pages []Page
	var cur strings.Builder
	for _, para := range strings.Split(text, "\n") {
		if cur.Len() > 0 && cur.Len()+len(para) > size {
			pages = append(pages, Page{N: len(pages) + 1, Text: strings.TrimSpace(cur.String())})
			cur.Reset()
		}
		cur.WriteString(para)
		cur.WriteByte('\n')
		for cur.Len() > size*2 { // one enormous paragraph
			s := cur.String()
			pages = append(pages, Page{N: len(pages) + 1, Text: strings.TrimSpace(s[:size])})
			cur.Reset()
			cur.WriteString(s[size:])
		}
	}
	if strings.TrimSpace(cur.String()) != "" {
		pages = append(pages, Page{N: len(pages) + 1, Text: strings.TrimSpace(cur.String())})
	}
	return pages
}
