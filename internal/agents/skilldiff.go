package agents

import (
	"context"

	"archive/zip"
	"bytes"
	"fmt"
	"github.com/google/uuid"
	"io"
	"path"
	"sort"
	"strings"
)

// What changes when a skill of the same name is imported again, shown
// before anything on the shelf is replaced (OpenClaw plan, step 2).

// DiffLine is one line of an instructions diff.
type DiffLine struct {
	Op   string `json:"op"` // "+", "-" or " " (context)
	Text string `json:"text"`
}

// FilesDiff is the inventory change by path and hash.
type FilesDiff struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	Changed []string `json:"changed"`
}

// SkillDiff is the difference between the shelf's skill and a new import.
type SkillDiff struct {
	Same         bool       `json:"same"`
	TitleChanged bool       `json:"title_changed"`
	DescChanged  bool       `json:"description_changed"`
	Instructions []DiffLine `json:"instructions"`
	// InstructionsChanged is false when the SKILL.md body is identical.
	InstructionsChanged bool      `json:"instructions_changed"`
	Files               FilesDiff `json:"files"`
	// ScriptsAdded lists scripts that are new or changed: what an agent
	// could run that it could not run before.
	ScriptsAdded []string `json:"scripts_added"`
	// NewFindings are the findings on files that are new or changed.
	NewFindings []string `json:"new_findings"`
	// HadScripts and HasScripts say whether the skill gained scripts.
	HadScripts bool `json:"had_scripts"`
	HasScripts bool `json:"has_scripts"`
}

const maxDiffLines = 300

// DiffSkill compares a shelf skill with a new import.
func DiffSkill(old StoredSkill, imp *SkillFolderImport) *SkillDiff {
	d := &SkillDiff{
		Files:        FilesDiff{Added: []string{}, Removed: []string{}, Changed: []string{}},
		ScriptsAdded: []string{}, NewFindings: []string{}, Instructions: []DiffLine{},
		HadScripts: old.HasScripts, HasScripts: imp.HasScripts,
		TitleChanged: old.Title != imp.Skill.Preset.Title, DescChanged: old.Description != imp.Skill.Preset.Description,
	}
	d.Instructions, d.InstructionsChanged = lineDiff(old.Instructions, imp.Skill.Body)
	oldBy := map[string]SkillFile{}
	for _, f := range old.Files {
		oldBy[f.Path] = f
	}
	newBy := map[string]SkillFile{}
	for _, f := range imp.Files {
		newBy[f.Path] = f
		o, had := oldBy[f.Path]
		switch {
		case !had:
			d.Files.Added = append(d.Files.Added, f.Path)
		case o.SHA256 != f.SHA256:
			d.Files.Changed = append(d.Files.Changed, f.Path)
		default:
			continue
		}
		if f.Kind == FileScript {
			d.ScriptsAdded = append(d.ScriptsAdded, f.Path)
		}
		for _, x := range f.Findings {
			d.NewFindings = append(d.NewFindings, f.Path+": "+x)
		}
	}
	for p := range oldBy {
		if _, ok := newBy[p]; !ok {
			d.Files.Removed = append(d.Files.Removed, p)
		}
	}
	sort.Strings(d.Files.Removed)
	d.Same = !d.TitleChanged && !d.DescChanged && !d.InstructionsChanged &&
		len(d.Files.Added) == 0 && len(d.Files.Removed) == 0 && len(d.Files.Changed) == 0
	return d
}

// lineDiff is a line-based LCS diff with two lines of context, capped.
func lineDiff(a, b string) ([]DiffLine, bool) {
	if a == b {
		return []DiffLine{}, false
	}
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	n, m := len(x), len(y)
	// lcs[i][j] is the LCS length of x[i:], y[j:].
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var all []DiffLine
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case x[i] == y[j]:
			all = append(all, DiffLine{" ", x[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			all = append(all, DiffLine{"-", x[i]})
			i++
		default:
			all = append(all, DiffLine{"+", y[j]})
			j++
		}
	}
	for ; i < n; i++ {
		all = append(all, DiffLine{"-", x[i]})
	}
	for ; j < m; j++ {
		all = append(all, DiffLine{"+", y[j]})
	}
	// Keep changed lines and two lines of context around them.
	keep := make([]bool, len(all))
	for k, l := range all {
		if l.Op != " " {
			for c := max(0, k-2); c <= min(len(all)-1, k+2); c++ {
				keep[c] = true
			}
		}
	}
	out := []DiffLine{}
	for k, l := range all {
		if keep[k] {
			out = append(out, l)
			if len(out) >= maxDiffLines {
				out = append(out, DiffLine{" ", "… (the diff is cut here)"})
				break
			}
		}
	}
	return out, true
}

// ---- bulk import: a zip of a skills/ directory ----

// Limits on a bulk archive. The declared sizes are checked before anything
// is read, and the bytes actually copied are counted too, so an archive that
// lies about its sizes cannot grow memory past them.
const (
	MaxBulkZipBytes   = 20 << 20
	MaxBulkSkills     = 50
	MaxBulkFiles      = MaxSkillFiles * MaxBulkSkills // entries in the whole archive
	MaxBulkTotalBytes = 100 << 20                     // uncompressed, whole archive
)

// BulkEntry is one skill folder found in a bulk archive, as its own
// single-skill zip. A folder over a limit has Err set and no Zip: it is
// refused on its own and the others still go through.
type BulkEntry struct {
	// Dir is the folder's path inside the archive (its identity for the
	// approve and skip choice).
	Dir string
	Zip []byte
	Err string
}

// SplitBulk finds every skill folder in a zip of a skills directory (each a
// folder with a SKILL.md, at most two levels down, optionally under a
// "skills/" prefix) and returns each as its own zip, so each is read, shown
// and approved on its own and one hostile skill cannot ride in with the
// rest.
func SplitBulk(zipData []byte) ([]BulkEntry, error) {
	if len(zipData) == 0 {
		return nil, fmt.Errorf("%w: the archive is empty", ErrSkill)
	}
	if len(zipData) > MaxBulkZipBytes {
		return nil, fmt.Errorf("%w: the archive is larger than %d MB", ErrSkill, MaxBulkZipBytes>>20)
	}
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil, fmt.Errorf("%w: not a zip archive", ErrSkill)
	}
	// Whole-archive limits, on what the entries declare, before any is read.
	if len(zr.File) > MaxBulkFiles {
		return nil, fmt.Errorf("%w: more than %d entries in the archive", ErrSkill, MaxBulkFiles)
	}
	var declared uint64
	for _, f := range zr.File {
		declared += f.UncompressedSize64
		if declared > MaxBulkTotalBytes {
			return nil, fmt.Errorf("%w: the archive is larger than %d MB uncompressed", ErrSkill, MaxBulkTotalBytes>>20)
		}
	}
	roots := map[string]bool{}
	for _, f := range zr.File {
		name := path.Clean(strings.ReplaceAll(f.Name, "\\", "/"))
		if path.Base(name) != "SKILL.md" || skipEntry(name) {
			continue
		}
		dir := path.Dir(name)
		if strings.Count(dir, "/") > 1 || strings.HasPrefix(dir, "/") || dir == ".." || strings.HasPrefix(dir, "../") {
			continue
		}
		roots[dir] = true
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("%w: no skill folder (a folder with a SKILL.md) in the archive", ErrSkill)
	}
	// A root that is inside another root belongs to it (a skill's own
	// subfolder is not a second skill).
	dirs := make([]string, 0, len(roots))
	for d := range roots {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	var top []string
	for _, d := range dirs {
		inside := false
		for _, t := range top {
			if t != "." && strings.HasPrefix(d, t+"/") {
				inside = true
			}
		}
		if !inside {
			top = append(top, d)
		}
	}
	if len(top) > MaxBulkSkills {
		return nil, fmt.Errorf("%w: %d skills in one archive; at most %d at a time", ErrSkill, len(top), MaxBulkSkills)
	}

	// Which entries belong to which folder, with the declared size, so a
	// folder over its limits is refused before a byte of it is copied.
	type member struct {
		f   *zip.File
		rel string
	}
	members := map[string][]member{}
	declaredBy := map[string]uint64{}
	for _, f := range zr.File {
		name := strings.ReplaceAll(f.Name, "\\", "/")
		clean := path.Clean(name)
		if strings.HasSuffix(name, "/") || skipEntry(clean) {
			continue
		}
		for _, dir := range top {
			var rel string
			if dir == "." {
				rel = clean
			} else if strings.HasPrefix(clean, dir+"/") {
				rel = strings.TrimPrefix(clean, dir+"/")
			} else {
				continue
			}
			members[dir] = append(members[dir], member{f, rel})
			declaredBy[dir] += f.UncompressedSize64
			break
		}
	}

	var out []BulkEntry
	var copied int64
	for _, dir := range top {
		ms := members[dir]
		switch {
		case len(ms) > MaxSkillFiles:
			out = append(out, BulkEntry{Dir: dir, Err: fmt.Sprintf("more than %d files", MaxSkillFiles)})
			continue
		case declaredBy[dir] > MaxSkillTotalBytes:
			out = append(out, BulkEntry{Dir: dir, Err: fmt.Sprintf("larger than %d MB uncompressed", MaxSkillTotalBytes>>20)})
			continue
		}
		big := ""
		for _, m := range ms {
			if m.f.UncompressedSize64 > MaxSkillFileBytes {
				big = m.rel
				break
			}
		}
		if big != "" {
			out = append(out, BulkEntry{Dir: dir, Err: fmt.Sprintf("%s is larger than %d MB", big, MaxSkillFileBytes>>20)})
			continue
		}
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		var folderBytes int64
		refused := ""
		for _, m := range ms {
			// Symlinks, absolute and parent paths are left for
			// ImportSkillFolder to refuse, so the same rules apply: keep the
			// entry as is.
			hdr := m.f.FileHeader
			hdr.Name = m.rel
			w, err := zw.CreateHeader(&hdr)
			if err != nil {
				return nil, err
			}
			rc, err := m.f.Open()
			if err != nil {
				return nil, fmt.Errorf("%w: cannot read %q", ErrSkill, m.f.Name)
			}
			n, err := io.Copy(w, io.LimitReader(rc, MaxSkillFileBytes+1))
			rc.Close()
			if err != nil {
				return nil, err
			}
			folderBytes += n
			copied += n
			if n > MaxSkillFileBytes {
				refused = fmt.Sprintf("%s is larger than %d MB", m.rel, MaxSkillFileBytes>>20)
				break
			}
			if folderBytes > MaxSkillTotalBytes {
				refused = fmt.Sprintf("larger than %d MB uncompressed", MaxSkillTotalBytes>>20)
				break
			}
			if copied > MaxBulkTotalBytes {
				return nil, fmt.Errorf("%w: the archive is larger than %d MB uncompressed", ErrSkill, MaxBulkTotalBytes>>20)
			}
		}
		if refused != "" {
			out = append(out, BulkEntry{Dir: dir, Err: refused})
			continue
		}
		if err := zw.Close(); err != nil {
			return nil, err
		}
		out = append(out, BulkEntry{Dir: dir, Zip: buf.Bytes()})
	}
	return out, nil
}

// BulkItem is one skill of a bulk import, with its own review.
type BulkItem struct {
	Dir     string        `json:"dir"`
	Preview *SkillPreview `json:"preview,omitempty"`
	// Error is why this folder could not be read; the others still can.
	Error string `json:"error,omitempty"`
}

// BulkPreview reads every skill folder in a zip and previews each against
// the shelf. A folder that cannot be read says why and does not stop the
// others.
func (s *SkillStore) BulkPreview(ctx context.Context, owner uuid.UUID, zipData []byte) ([]BulkItem, error) {
	entries, err := SplitBulk(zipData)
	if err != nil {
		return nil, err
	}
	out := make([]BulkItem, 0, len(entries))
	for _, e := range entries {
		it := BulkItem{Dir: e.Dir}
		if e.Err != "" {
			it.Error = e.Err
			out = append(out, it)
			continue
		}
		pv, err := s.Preview(ctx, owner, e.Zip)
		if err != nil {
			it.Error = strings.TrimPrefix(err.Error(), ErrSkill.Error()+": ")
		} else {
			it.Preview = pv
		}
		out = append(out, it)
	}
	return out, nil
}

// BulkApprove approves only the chosen folders (by Dir); the rest are
// skipped, and a folder named that is not in the archive is an error.
func (s *SkillStore) BulkApprove(ctx context.Context, owner, approver uuid.UUID, zipData []byte, dirs []string) ([]StoredSkill, []BulkItem, error) {
	entries, err := SplitBulk(zipData)
	if err != nil {
		return nil, nil, err
	}
	by := map[string]BulkEntry{}
	for _, e := range entries {
		by[e.Dir] = e
	}
	done := []StoredSkill{}
	failed := []BulkItem{}
	for _, d := range dirs {
		e, ok := by[d]
		if !ok {
			failed = append(failed, BulkItem{Dir: d, Error: "not in the archive"})
			continue
		}
		if e.Err != "" {
			failed = append(failed, BulkItem{Dir: d, Error: e.Err})
			continue
		}
		k, err := s.Approve(ctx, owner, approver, e.Zip)
		if err != nil {
			failed = append(failed, BulkItem{Dir: d, Error: strings.TrimPrefix(err.Error(), ErrSkill.Error()+": ")})
			continue
		}
		done = append(done, *k)
	}
	return done, failed, nil
}
