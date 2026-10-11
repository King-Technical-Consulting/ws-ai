package agents

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Skill folders (OpenClaw plan, step 1). A skill folder is a SKILL.md with
// optional scripts/, references/ and assets: the AgentSkills layout. This
// file reads one from a zip into an inventory and a list of findings. It
// never executes, stores or grants anything: the person reads it, and a
// later step stores an approved folder and runs scripts only inside a code
// project's sandbox, under the agent's tool policy.

// Limits on an uploaded skill folder.
const (
	MaxSkillZipBytes   = 5 << 20
	MaxSkillFiles      = 200
	MaxSkillFileBytes  = 2 << 20
	MaxSkillTotalBytes = 20 << 20
	// scanBytes is how much of a text file is read for findings.
	scanBytes = 256 << 10
)

// File kinds in an inventory.
const (
	FileScript    = "script"
	FileReference = "reference"
	FileAsset     = "asset"
	FileOther     = "other"
)

// SkillFile is one file of the folder, as inventoried.
type SkillFile struct {
	Path        string `json:"path"`
	Kind        string `json:"kind"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	Interpreter string `json:"interpreter,omitempty"`
	Binary      bool   `json:"binary,omitempty"`
	// Refused is true for a file holding a string shaped like a literal
	// credential: it is listed, never stored and never put in a sandbox.
	Refused  bool     `json:"refused,omitempty"`
	Findings []string `json:"findings"`
}

// SkillFolderImport is the preview of an uploaded skill folder.
type SkillFolderImport struct {
	// Skill is the SKILL.md read as a single-file import: the preset, the
	// tools it asked for (none granted) and its warnings.
	Skill SkillImport `json:"skill"`
	Files []SkillFile `json:"files"`
	// HasScripts is true when the folder carries anything runnable. A
	// skill without scripts is instructions only and works in any agent.
	HasScripts bool `json:"has_scripts"`
	// UsableAnywhere is !HasScripts.
	UsableAnywhere bool `json:"usable_anywhere"`
	// RefusedSecrets are front matter entries that name credentials
	// (keys only, never values). They are not stored.
	RefusedSecrets []string `json:"refused_secrets"`
	// Findings are folder-level notes: a file that was skipped, a layout
	// that was unusual.
	Findings []string `json:"findings"`
	// Bytes is the total uncompressed size read.
	Bytes int64 `json:"bytes"`
	// Contents holds the bytes of every inventoried file that was not
	// refused, by path. It never leaves the server: approval stores it.
	Contents map[string][]byte `json:"-"`
}

var (
	shebangRe = regexp.MustCompile(`\A#!\s*(?:/usr/bin/env\s+)?(\S+)`)
	// networkRe: commands that fetch or install from the network.
	networkRe = regexp.MustCompile(`(?i)\b(?:curl|wget|nc|ncat|scp|rsync|ssh|ftp|telnet)\b|\b(?:pip3?|npm|yarn|pnpm|gem|cargo|go)\s+(?:install|get|add)\b|\bgit\s+clone\b|\bapt(?:-get)?\s+install\b|\brequests\.(?:get|post)\b|\burllib\b|\bfetch\(|\bhttp\.client\b|\bnet/http\b|\bhttps?://`)
	// riskyRe: shell that is hard to review or destroys things.
	riskyRe = regexp.MustCompile(`(?i)\bsudo\b|\brm\s+-[a-z]*r[a-z]*f?\s+(?:/|~|\$HOME)|\bchmod\s+(?:-R\s+)?[0-7]*777\b|\beval\b|\bbase64\s+(?:-d|--decode)\b[^|\n]*\|\s*(?:ba)?sh\b|\|\s*(?:ba)?sh\b|\bexec\s*\(|\bos\.system\b|\bsubprocess\b|\bmkfs\b|\bdd\s+if=|>\s*/dev/sd`)
	// secretLiteralRe matches strings shaped like a credential itself; secretEnvRe
	// matches code that reads one from the environment.
	secretLiteralRe = regexp.MustCompile(`(?i)AKIA[0-9A-Z]{16}|\bghp_[A-Za-z0-9]{20,}|\bgithub_pat_[A-Za-z0-9_]{20,}|\bsk-[A-Za-z0-9_-]{20,}|\bxox[abprs]-[A-Za-z0-9-]{10,}|-----BEGIN [A-Z ]*PRIVATE KEY-----|\b(?:api[_-]?key|secret|token|password|passwd)\b\s*[:=]\s*["']?[A-Za-z0-9_\-./+=]{12,}`)
	secretEnvRe     = regexp.MustCompile(`(?i)\$\{?[A-Z0-9_]*(?:API_?KEY|SECRET|TOKEN|PASSWORD)[A-Z0-9_]*\}?|os\.environ(?:\.get)?\W+["'][A-Z0-9_]*(?:KEY|SECRET|TOKEN|PASSWORD)[A-Z0-9_]*["']|process\.env\.[A-Z0-9_]*(?:KEY|SECRET|TOKEN|PASSWORD)`)
	// secretKeyRe names front matter keys that carry credentials.
	secretKeyRe = regexp.MustCompile(`(?i)^(?:env|environment|api[_-]?keys?|secrets?|credentials?|tokens?|passwords?|auth)$`)
	scriptExt   = map[string]string{".sh": "bash", ".bash": "bash", ".zsh": "zsh", ".py": "python", ".js": "node", ".mjs": "node", ".ts": "ts-node", ".rb": "ruby", ".pl": "perl", ".php": "php", ".ps1": "powershell", ".bat": "cmd", ".cmd": "cmd", ".lua": "lua"}
	assetExt    = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".svg": true, ".ico": true, ".pdf": true, ".csv": true, ".json": true, ".yaml": true, ".yml": true, ".toml": true, ".txt": false}
)

// ImportSkillFolder reads a zip holding one skill folder. SKILL.md is at
// the archive root or in its only top-level directory.
func ImportSkillFolder(zipData []byte) (*SkillFolderImport, error) {
	if len(zipData) == 0 {
		return nil, fmt.Errorf("%w: the archive is empty", ErrSkill)
	}
	if len(zipData) > MaxSkillZipBytes {
		return nil, fmt.Errorf("%w: the archive is larger than %d MB", ErrSkill, MaxSkillZipBytes>>20)
	}
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil, fmt.Errorf("%w: not a zip archive", ErrSkill)
	}
	type entry struct {
		name string
		f    *zip.File
	}
	var entries []entry
	seen := map[string]bool{}
	var total int64
	for _, f := range zr.File {
		name := strings.ReplaceAll(f.Name, "\\", "/")
		if strings.HasSuffix(name, "/") {
			continue
		}
		if f.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: %q is a symbolic link, which a skill may not contain", ErrSkill, f.Name)
		}
		clean := path.Clean(name)
		if strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") || path.IsAbs(name) || len(name) > 0 && name[0] == '/' {
			return nil, fmt.Errorf("%w: %q leaves the folder", ErrSkill, f.Name)
		}
		if skipEntry(clean) {
			continue
		}
		if seen[clean] {
			return nil, fmt.Errorf("%w: %q appears twice", ErrSkill, clean)
		}
		seen[clean] = true
		if f.UncompressedSize64 > MaxSkillFileBytes {
			return nil, fmt.Errorf("%w: %q is larger than %d MB", ErrSkill, clean, MaxSkillFileBytes>>20)
		}
		total += int64(f.UncompressedSize64)
		if total > MaxSkillTotalBytes {
			return nil, fmt.Errorf("%w: the folder is larger than %d MB uncompressed", ErrSkill, MaxSkillTotalBytes>>20)
		}
		entries = append(entries, entry{clean, f})
		if len(entries) > MaxSkillFiles {
			return nil, fmt.Errorf("%w: more than %d files", ErrSkill, MaxSkillFiles)
		}
	}
	// Find SKILL.md and the folder root.
	var roots []string
	for _, e := range entries {
		if path.Base(e.name) == "SKILL.md" && strings.Count(e.name, "/") <= 1 {
			roots = append(roots, path.Dir(e.name))
		}
	}
	sort.Strings(roots)
	switch {
	case len(roots) == 0:
		return nil, fmt.Errorf("%w: no SKILL.md at the top of the archive or in its one top-level folder", ErrSkill)
	case len(roots) > 1:
		return nil, fmt.Errorf("%w: %d skills in one archive (%s); upload one skill folder at a time", ErrSkill, len(roots), strings.Join(roots, ", "))
	}
	root := roots[0]
	prefix := ""
	if root != "." {
		prefix = root + "/"
	}

	out := &SkillFolderImport{Files: []SkillFile{}, RefusedSecrets: []string{}, Findings: []string{}, Contents: map[string][]byte{}}
	var skillSrc []byte
	for _, e := range entries {
		if !strings.HasPrefix(e.name, prefix) {
			out.Findings = append(out.Findings, fmt.Sprintf("%s is outside the skill folder and was ignored", e.name))
			continue
		}
		rel := strings.TrimPrefix(e.name, prefix)
		rc, err := e.f.Open()
		if err != nil {
			return nil, fmt.Errorf("%w: cannot read %q", ErrSkill, rel)
		}
		data, err := io.ReadAll(io.LimitReader(rc, MaxSkillFileBytes+1))
		rc.Close()
		if err != nil || int64(len(data)) > MaxSkillFileBytes {
			return nil, fmt.Errorf("%w: %q is larger than %d MB", ErrSkill, rel, MaxSkillFileBytes>>20)
		}
		out.Bytes += int64(len(data))
		if rel == "SKILL.md" {
			skillSrc = data
			continue
		}
		sf := inventoryFile(rel, data, e.f.Mode())
		out.Files = append(out.Files, sf)
		if !sf.Refused {
			out.Contents[rel] = data
		} else {
			out.Findings = append(out.Findings, rel+" holds a string shaped like a credential and is not stored")
		}
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Path < out.Files[j].Path })
	skill, err := ImportSkill(skillSrc)
	if err != nil {
		return nil, err
	}
	out.Skill = *skill
	out.RefusedSecrets = frontMatterSecrets(skillSrc)
	if len(out.RefusedSecrets) > 0 {
		out.Skill.Warnings = append(out.Skill.Warnings, "the skill names credentials in its front matter ("+strings.Join(out.RefusedSecrets, ", ")+"); they are not stored and nothing is injected. Give an agent a secret only through ws's own secret handling")
	}
	for _, f := range out.Files {
		if f.Kind == FileScript {
			out.HasScripts = true
		}
	}
	out.UsableAnywhere = !out.HasScripts
	if out.HasScripts {
		out.Findings = append(out.Findings, "the folder carries scripts: they are never run by the import, and an agent can run them only inside a code project's sandbox through bash, under the tool policy")
	}
	return out, nil
}

// skipEntry drops archive noise that is not part of a skill.
func skipEntry(clean string) bool {
	if strings.HasPrefix(clean, "__MACOSX/") || strings.Contains(clean, "/__MACOSX/") {
		return true
	}
	b := path.Base(clean)
	return b == ".DS_Store" || b == "Thumbs.db" || strings.HasPrefix(b, "._")
}

// inventoryFile classifies one file and scans it. Text is scanned up to a
// limit; a binary is only counted and hashed.
func inventoryFile(rel string, data []byte, mode fs.FileMode) SkillFile {
	sum := sha256.Sum256(data)
	f := SkillFile{Path: rel, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Findings: []string{}}
	ext := strings.ToLower(path.Ext(rel))
	f.Binary = bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data[:min(len(data), scanBytes)])
	top := strings.SplitN(rel, "/", 2)[0]
	var shebang string
	if !f.Binary {
		if m := shebangRe.FindStringSubmatch(string(data[:min(len(data), 200)])); m != nil {
			shebang = path.Base(m[1])
		}
	}
	switch {
	case top == "scripts" || scriptExt[ext] != "" || shebang != "" || f.Binary && mode.Perm()&0o111 != 0:
		f.Kind = FileScript
	case top == "references" || top == "docs" || top == "examples" || ext == ".md" || ext == ".txt" || ext == ".rst":
		f.Kind = FileReference
	case top == "assets" || top == "templates" || assetExt[ext]:
		f.Kind = FileAsset
	default:
		f.Kind = FileOther
	}
	if f.Kind == FileScript {
		switch {
		case shebang != "":
			f.Interpreter = shebang
		case scriptExt[ext] != "":
			f.Interpreter = scriptExt[ext]
		}
		if f.Binary {
			f.Findings = append(f.Findings, "a binary or executable file: it cannot be reviewed as text")
		}
	}
	if f.Binary {
		return f
	}
	text := string(data[:min(len(data), scanBytes)])
	if _, hidden := stripHidden(text); hidden > 0 {
		f.Findings = append(f.Findings, fmt.Sprintf("%d hidden character(s) (zero-width, direction-override or control)", hidden))
	}
	if f.Kind == FileScript {
		if networkRe.MatchString(text) {
			f.Findings = append(f.Findings, "uses the network or installs packages: "+uniqueMatches(networkRe, text, 4))
		}
		if riskyRe.MatchString(text) {
			f.Findings = append(f.Findings, "runs hard-to-review or destructive commands: "+uniqueMatches(riskyRe, text, 4))
		}
	} else if f.Kind == FileReference || f.Kind == FileOther {
		if overrideRe.MatchString(text) {
			f.Findings = append(f.Findings, "contains instruction-override language")
		}
		if hosts := linkHosts(text); len(hosts) > 0 {
			f.Findings = append(f.Findings, "links to: "+strings.Join(hosts, ", "))
		}
	}
	// A secret-shaped string is reported by place and kind, never quoted.
	// A literal credential gets the file refused; a read of a secret
	// environment variable is only noted, since none is ever provided.
	if n := len(secretLiteralRe.FindAllString(text, -1)); n > 0 {
		f.Refused = true
		f.Findings = append(f.Findings, fmt.Sprintf("%d string(s) shaped like a literal credential: the file is not stored", n))
	}
	if n := len(secretEnvRe.FindAllString(text, -1)); n > 0 {
		f.Findings = append(f.Findings, fmt.Sprintf("%d read(s) of a secret environment variable; no secret is provided inside a sandbox", n))
	}
	return f
}

// uniqueMatches lists up to n distinct matches, lowercased and clipped, for
// a finding line. It is used only for commands and hosts, never for secrets.
func uniqueMatches(re *regexp.Regexp, text string, n int) string {
	seen := map[string]bool{}
	var out []string
	for _, m := range re.FindAllString(text, -1) {
		m = strings.ToLower(strings.TrimSpace(m))
		if len(m) > 40 {
			m = m[:40]
		}
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
			if len(out) == n {
				break
			}
		}
	}
	return strings.Join(out, ", ")
}

// frontMatterSecrets lists the front matter keys that carry credentials,
// by name only: "env", or "env.OPENAI_API_KEY" for the names inside an env
// map. Values are never returned.
func frontMatterSecrets(src []byte) []string {
	text, _ := stripHidden(strings.ReplaceAll(string(src), "\r\n", "\n"))
	m := frontmatterRe.FindStringSubmatchIndex(text)
	if m == nil {
		return []string{}
	}
	var raw map[string]any
	if yaml.Unmarshal([]byte(text[m[2]:m[3]]), &raw) != nil {
		return []string{}
	}
	var out []string
	var walk func(prefix string, v map[string]any, depth int)
	walk = func(prefix string, v map[string]any, depth int) {
		for k, val := range v {
			name := prefix + k
			if secretKeyRe.MatchString(k) {
				out = append(out, name)
				if sub, ok := val.(map[string]any); ok {
					for sk := range sub {
						out = append(out, name+"."+sk)
					}
				}
				continue
			}
			if sub, ok := val.(map[string]any); ok && depth < 3 {
				walk(name+".", sub, depth+1)
			}
		}
	}
	walk("", raw, 0)
	out = uniqueSorted(out)
	if out == nil {
		out = []string{}
	}
	return out
}
