package artifacts

import (
	"bytes"
	"fmt"
	"html"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	goldhtml "github.com/yuin/goldmark/renderer/html"

	"github.com/jking323/ws/internal/store"
)

// OriginHandler serves artifact versions on the separate artifact origin.
// The parent embeds them with <iframe sandbox="allow-scripts"> (never
// allow-same-origin), so the document runs with an opaque origin: no
// cookies, no storage, no access to the parent. CSP blocks network calls.
func (s *Service) OriginHandler(appOrigin string) http.Handler {
	r := chi.NewRouter()
	r.Get("/a/{version}", func(w http.ResponseWriter, req *http.Request) {
		id, err := uuid.Parse(chi.URLParam(req, "version"))
		if err != nil || !s.Verify(id, req.URL.Query().Get("t")) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		v, err := s.DB.GetArtifactVersionByID(req.Context(), id)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		a, err := s.DB.GetArtifact(req.Context(), v.ArtifactID)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		content := ""
		if v.Content != nil {
			content = *v.Content
		}
		body := Render(Kind(a.Kind), a.Title, langOf(a), content)
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Content-Security-Policy", CSP(appOrigin))
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "private, max-age=300")
		h.Set("Cross-Origin-Resource-Policy", "cross-origin")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(body))
	})
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "not found", 404) })
	return r
}

// CSP is the artifact document policy. Scripts may be inline or from the
// two CDNs (Mermaid, Tailwind CDN, React for M6). No connect-src, so
// fetch/XHR/WebSocket are blocked; images only from data: and blob: so a
// generated page cannot beacon conversation content out. frame-ancestors
// pins embedding to the app origin.
func CSP(appOrigin string) string {
	return strings.Join([]string{
		"default-src 'none'",
		"script-src 'unsafe-inline' 'unsafe-eval' https://cdnjs.cloudflare.com https://cdn.jsdelivr.net https://cdn.tailwindcss.com https://unpkg.com",
		"style-src 'unsafe-inline' https://cdnjs.cloudflare.com https://cdn.jsdelivr.net https://fonts.googleapis.com",
		"font-src data: https://fonts.gstatic.com https://cdnjs.cloudflare.com",
		"img-src data: blob:",
		"media-src data: blob:",
		"connect-src 'none'",
		"form-action 'none'",
		"base-uri 'none'",
		"frame-ancestors " + appOrigin,
	}, "; ")
}

func langOf(a store.Artifact) string {
	if a.Language != nil {
		return *a.Language
	}
	return ""
}

// runtime is injected into every document: forwards errors and the
// document height to the parent over postMessage. The parent validates
// event.source against its iframe (origin is "null" under sandbox).
const runtime = `<script>(function(){
var post=function(m){try{parent.postMessage(Object.assign({ws:"artifact"},m),"*")}catch(e){}};
window.addEventListener("error",function(e){post({type:"error",message:String(e.message||e)})});
window.addEventListener("unhandledrejection",function(e){post({type:"error",message:String(e.reason&&e.reason.message||e.reason||"rejection")})});
var ro=function(){post({type:"height",height:document.documentElement.scrollHeight})};
window.addEventListener("load",function(){ro();if(window.ResizeObserver){new ResizeObserver(ro).observe(document.documentElement)}});
document.addEventListener("click",function(e){var a=e.target&&e.target.closest&&e.target.closest("a[href]");if(a&&/^https?:/.test(a.href)){e.preventDefault();post({type:"open",href:a.href})}},true);
})();</script>`

const baseCSS = `<style>
:root{color-scheme:light dark}
html,body{margin:0;background:#faf8f5;color:#2c2416}
@media (prefers-color-scheme:dark){html,body{background:#1b1611;color:#eee6d9}}
body{font-family:Georgia,"Bitstream Charter","Times New Roman",serif;font-size:17px;line-height:1.65;padding:24px;word-spacing:.05em}
pre,code{font-family:ui-monospace,SFMono-Regular,Menlo,Monaco,Consolas,monospace}
pre{background:rgba(127,127,127,.12);border-radius:2px;padding:.8em 1em;overflow-x:auto;font-size:.82em;line-height:1.5}
code:not(pre code){background:rgba(127,127,127,.12);padding:.1em .3em;border-radius:2px;font-size:.85em}
a{color:#b85a1a;text-decoration:none;border-bottom:1px dotted #c9a882}
h1{font-size:1.6em;font-weight:400;letter-spacing:.02em;line-height:1.25}
h2{font-size:1em;font-weight:400;font-variant:small-caps;letter-spacing:.15em;opacity:.7;border-bottom:1px solid rgba(127,127,127,.3);padding-bottom:.2em}
h3{font-size:1em;font-weight:400;font-style:italic}
blockquote{border-left:1px dotted currentColor;opacity:.85;margin:1.2em 0;padding:0 1.5em}
table{border-collapse:collapse}td,th{border:1px solid rgba(127,127,127,.4);padding:.3em .6em}
img,svg{max-width:100%}
.ws-svg{display:grid;place-items:center;min-height:calc(100vh - 48px)}
</style>`

var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM, extension.Footnote),
	goldmark.WithRendererOptions(goldhtml.WithHardWraps()),
)

// Render builds the full HTML document for a version. Markdown is
// rendered server-side with raw HTML escaped. Code is escaped into a
// <pre>. Mermaid loads the renderer from cdnjs.
func Render(kind Kind, title, language, content string) string {
	t := html.EscapeString(title)
	switch kind {
	case KindHTML, KindDesign:
		return injectRuntime(content)
	case KindSVG:
		return fmt.Sprintf(`<!doctype html><html><head><meta charset="utf-8"><title>%s</title>%s%s</head><body><div class="ws-svg">%s</div></body></html>`,
			t, baseCSS, runtime, sanitizeSVG(content))
	case KindMarkdown:
		var buf bytes.Buffer
		if err := md.Convert([]byte(content), &buf); err != nil {
			buf.Reset()
			buf.WriteString("<pre>" + html.EscapeString(content) + "</pre>")
		}
		return fmt.Sprintf(`<!doctype html><html><head><meta charset="utf-8"><title>%s</title>%s%s</head><body><article>%s</article></body></html>`,
			t, baseCSS, runtime, buf.String())
	case KindMermaid:
		return fmt.Sprintf(`<!doctype html><html><head><meta charset="utf-8"><title>%s</title>%s%s
<script src="https://cdnjs.cloudflare.com/ajax/libs/mermaid/11.4.1/mermaid.min.js"></script>
<script>window.addEventListener("load",function(){var dark=matchMedia("(prefers-color-scheme: dark)").matches;mermaid.initialize({startOnLoad:true,theme:dark?"dark":"neutral",securityLevel:"strict"})});</script>
</head><body><pre class="mermaid" style="background:none;font-size:inherit">%s</pre></body></html>`,
			t, baseCSS, runtime, html.EscapeString(content))
	default: // code
		return fmt.Sprintf(`<!doctype html><html><head><meta charset="utf-8"><title>%s</title>%s%s</head><body style="padding:0"><pre style="margin:0;border-radius:0;min-height:100vh"><code class="language-%s">%s</code></pre></body></html>`,
			t, baseCSS, runtime, html.EscapeString(language), html.EscapeString(content))
	}
}

// injectRuntime puts the runtime script at the top of <head>, or wraps a
// fragment in a document when the model returned only a body.
func injectRuntime(doc string) string {
	lower := strings.ToLower(doc)
	if i := strings.Index(lower, "<head>"); i >= 0 {
		return doc[:i+6] + `<meta charset="utf-8">` + runtime + doc[i+6:]
	}
	if i := strings.Index(lower, "<html"); i >= 0 {
		if j := strings.Index(lower[i:], ">"); j >= 0 {
			k := i + j + 1
			return doc[:k] + `<head><meta charset="utf-8">` + runtime + `</head>` + doc[k:]
		}
	}
	return `<!doctype html><html><head><meta charset="utf-8">` + runtime + `</head><body>` + doc + `</body></html>`
}

// sanitizeSVG strips script elements and on* handlers from an SVG. Scripts
// in artifacts are allowed in HTML kinds; an SVG artifact is a picture.
func sanitizeSVG(svg string) string {
	out := svg
	lower := strings.ToLower(out)
	for {
		i := strings.Index(lower, "<script")
		if i < 0 {
			break
		}
		j := strings.Index(lower[i:], "</script>")
		if j < 0 {
			out = out[:i]
			break
		}
		out = out[:i] + out[i+j+9:]
		lower = strings.ToLower(out)
	}
	// crude but effective: drop on*= attributes
	var b strings.Builder
	for _, tok := range strings.Split(out, " ") {
		tl := strings.ToLower(tok)
		if strings.HasPrefix(tl, "on") && strings.Contains(tl, "=") {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(tok)
	}
	return b.String()
}
