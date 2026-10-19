package filepreview

import (
	"bytes"
	"errors"
	"html"
	"html/template"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

var pageStyle = highlightCSS() + `
:root{color-scheme:light}*{box-sizing:border-box}
body{margin:0;color:#24292f;background:#f6f8fa;font:16px/1.7 system-ui,-apple-system,"Segoe UI",sans-serif}
main{max-width:1120px;margin:32px auto;padding:32px 40px;background:#fff;border:1px solid #d8dee4;border-radius:12px}
h1{font-size:1.5rem;line-height:1.35;overflow-wrap:anywhere;margin:0 0 12px}.meta{font-size:.85rem;color:#57606a;margin:0 0 28px}
a{color:#0969da;text-underline-offset:3px}a:focus-visible,summary:focus-visible,button:focus-visible{outline:2px solid #0969da;outline-offset:4px}
.file-kind{display:inline-block;color:#57606a;font-size:.75rem;line-height:1.5;border:1px solid #d8dee4;border-radius:5px;padding:2px 7px;margin-bottom:12px}
.html-preview{display:block;width:100%;height:100%;min-height:0;border:0;background:#fff}.preview-note{color:#57606a}
pre{max-width:100%;overflow:auto;background:#f6f8fa;padding:18px 20px;border:1px solid #eaeef2;border-radius:8px;line-height:1.6;tab-size:4}
code{font-family:ui-monospace,SFMono-Regular,Consolas,"Liberation Mono",monospace;font-size:.875rem}pre code{white-space:pre;overflow-wrap:normal}
.chroma{background:#f6f8fa}.source{padding:16px 0}.code-line{display:block;min-height:1.6em;padding-right:20px;scroll-margin-top:24px}.code-line:target{background:#fff1b8;box-shadow:inset 3px 0 #bf8700}
.source code{display:block;min-width:max-content}.line-no{display:inline-block;position:sticky;left:0;min-width:4em;padding:0 1em;text-align:right;color:#6e7781;background:#f6f8fa;text-decoration:none;user-select:none;font-variant-numeric:tabular-nums}.code-line:target .line-no{background:#fff1b8}
.markdown-body{width:100%;margin:0 auto;overflow-wrap:anywhere}.markdown-body h1,.markdown-body h2{padding-bottom:.4em;border-bottom:1px solid #d8dee4}.markdown-body h1{font-size:2rem}.markdown-body h2{font-size:1.45rem}.markdown-body h3{font-size:1.15rem}.markdown-body h1,.markdown-body h2,.markdown-body h3,.markdown-body h4{line-height:1.4;margin:1.7em 0 .7em}.markdown-body>:first-child{margin-top:0}
.markdown-body p,.markdown-body ul,.markdown-body ol,.markdown-body blockquote,.markdown-body pre,.markdown-body table{margin-top:0;margin-bottom:1.2em}.markdown-body li+li{margin-top:.3em}.markdown-body li>p{margin-bottom:.5em}.markdown-body ul,.markdown-body ol{padding-left:1.6em}
.markdown-body :not(pre)>code{padding:.15em .4em;background:#eff1f3;border-radius:4px;font-size:.875em}.markdown-body input[type=checkbox]{accent-color:#0969da;margin-right:.45em}
table{border-collapse:collapse;max-width:100%;display:block;overflow:auto;font-size:.94rem}td,th{padding:9px 14px;border:1px solid #d8dee4}th{font-weight:600;background:#f6f8fa}tbody tr:nth-child(even){background:#f6f8fa}
blockquote{margin-left:0;margin-right:0;padding:2px 18px;border-left:3px solid #d0d7de;color:#57606a}blockquote>:last-child{margin-bottom:0!important}hr{border:0;border-top:1px solid #d8dee4;margin:28px 0}
input[type=password]{display:block;width:100%;max-width:420px;padding:10px;font:inherit;margin:8px 0 16px;border:1px solid #8c959f;border-radius:6px}button{padding:9px 22px;background:#0969da;border:0;border-radius:6px;color:white;font:inherit;cursor:pointer}.error{color:#a42121}.login{max-width:480px;margin:64px auto}
details{margin-top:32px;border-top:1px solid #d8dee4;padding-top:20px}summary{cursor:pointer;color:#57606a;font-size:.9rem;width:fit-content}details[open]>summary{margin-bottom:16px}
.preview-shell{width:100%;max-width:none;min-height:100vh;margin:0;padding:0;border:0;border-radius:0}
.preview-header{display:flex;align-items:center;justify-content:space-between;gap:16px;min-width:0;padding:12px 24px;border-bottom:1px solid #d8dee4;background:#fff}.preview-title{min-width:0}.preview-heading{display:flex;align-items:center;gap:10px;min-width:0}.preview-heading h1{margin:0;min-width:0;overflow:hidden;white-space:nowrap;text-overflow:ellipsis;font-size:1rem}.preview-heading .file-kind{flex:none;margin:0;white-space:nowrap}.preview-header .meta{margin:4px 0 0;font-size:.75rem;line-height:1.5}.preview-note::before{content:" · "}
.reading-width{display:flex;align-items:center;gap:6px;flex:none;min-height:36px;color:#57606a;font-size:.85rem;cursor:pointer}.reading-width input{width:16px;height:16px;margin:0;accent-color:#0969da}.reading-width input:focus-visible{outline:2px solid #0969da;outline-offset:4px}
.preview-content{min-width:0}.preview-markdown .preview-content{padding:32px clamp(16px,3vw,56px)}.preview-markdown:has(#reading-width:checked) .markdown-body{max-width:90ch}.preview-markdown>.source-panel{margin:0 clamp(16px,3vw,56px) 32px}
.preview-code{display:flex;flex-direction:column;height:100vh;height:100dvh;min-height:0;overflow:hidden}.preview-code .preview-header{flex:none}.preview-code .preview-content{flex:1;min-height:0;overflow:hidden}.preview-code .source{height:100%;max-width:none;margin:0;border:0;border-radius:0}
.preview-html{display:grid;grid-template-columns:minmax(0,1fr) auto;grid-template-rows:auto minmax(0,1fr);height:100vh;height:100dvh;min-height:0;overflow:hidden}.preview-html .preview-header{grid-column:1;grid-row:1}.preview-html .source-panel{grid-column:2;grid-row:1;min-width:0;min-height:0;margin:0;padding:0;border:0;border-bottom:1px solid #d8dee4;background:#fff}.preview-html .source-panel>summary{display:flex;align-items:center;width:100%;height:100%;padding:12px 24px;white-space:nowrap;font-size:.8rem}.preview-html .source-panel>summary::before{content:"›";margin-right:8px;font-size:1.2rem;line-height:1}.preview-html .source-panel[open]{grid-column:1/-1;grid-row:2;overflow:auto}.preview-html .source-panel[open]>summary{position:sticky;top:0;z-index:1;height:auto;margin:0;border-bottom:1px solid #d8dee4;background:#fff}.preview-html .source-panel[open]>summary::before{content:"⌄"}.preview-html .source-panel .source{margin:0;border:0;border-radius:0}.html-view{grid-column:1/-1;grid-row:2;min-width:0;min-height:0;overflow:hidden}.preview-html .source-panel[open]+.html-view{display:none}
@media(hover:hover) and (pointer:fine){.preview-html .source-panel>summary:hover{background:#f6f8fa}}
@media(max-width:650px){main{margin:0;padding:24px 16px;border-radius:0;border:0}.login{margin:24px auto}.markdown-body h1{font-size:1.65rem}pre{padding:14px 12px}.source{padding:12px 0}.line-no{min-width:3.2em;padding:0 .65em}.meta{margin-bottom:24px}}
@media(max-width:650px){.preview-header{gap:10px;padding:10px 12px}.preview-heading{gap:8px}.preview-heading h1{font-size:.9rem}.preview-heading .file-kind{font-size:.7rem}.reading-width{min-height:44px;font-size:.8rem}.preview-markdown .preview-content{padding-top:24px}.preview-html{grid-template-columns:minmax(0,1fr);grid-template-rows:auto auto minmax(0,1fr)}.preview-html .source-panel{grid-column:1;grid-row:2}.preview-html .source-panel>summary{padding:6px 12px}.preview-html .source-panel[open]{grid-row:2/4}.html-view{grid-row:3}}
`

var loginTemplate = template.Must(template.New("login").Parse(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>文件预览登录</title><style>` + pageStyle + `</style></head><body><main class="login"><h1>文件预览</h1><p>输入预览口令后打开已分享的文件。</p>{{if .Error}}<p class="error" role="alert">{{.Error}}</p>{{end}}<form method="post" action=""><input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="next" value="{{.Next}}"><label for="password">预览口令</label><input type="password" id="password" name="password" autocomplete="current-password" maxlength="256" required autofocus><button type="submit">打开预览</button></form></main></body></html>`))

var previewTemplate = template.Must(template.New("preview").Parse(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Name}} · 文件预览</title><style>` + pageStyle + `</style></head><body><main class="preview-shell {{if .IsHTML}}preview-html{{else if .IsMarkdown}}preview-markdown{{else}}preview-code{{end}}">
<header class="preview-header"><div class="preview-title"><div class="preview-heading">{{if .IsMarkdown}}<span class="file-kind">Markdown 阅读</span>{{else if .IsHTML}}<span class="file-kind">HTML 静态预览</span>{{else if .Language}}<span class="file-kind">{{.Language}}</span>{{else}}<span class="file-kind">文本</span>{{end}}<h1 title="{{.Name}}">{{.Name}}</h1></div><p class="meta">当前文件内容 · 修改时间 {{.Modified}} · 每次打开读取最新内容{{if .IsHTML}}<span class="preview-note">{{if .HTMLDocument}}脚本、图片和外部资源不加载{{else}}无法生成 HTML 页面预览，请查看源码。{{end}}</span>{{end}}</p></div>{{if .IsMarkdown}}<label class="reading-width"><input id="reading-width" type="checkbox">舒适宽度</label>{{end}}</header>
{{if .IsMarkdown}}<div class="preview-content"><article class="markdown-body">{{.Markdown}}</article></div><details class="source-panel"><summary>查看 Markdown 源码 · 含行号</summary>{{.Source}}</details>
{{else if .IsHTML}}<details class="source-panel"{{if not .HTMLDocument}} open{{end}}><summary>查看 HTML 源码 · 含行号</summary>{{.Source}}</details>{{if .HTMLDocument}}<div class="html-view"><iframe class="html-preview" title="{{.Name}} 页面预览" sandbox="" referrerpolicy="no-referrer" srcdoc="{{.HTMLDocument}}"></iframe></div>{{end}}
{{else}}<div class="preview-content">{{.Source}}</div>{{end}}</main></body></html>`))

var messageTemplate = template.Must(template.New("message").Parse(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Title}}</title><style>` + pageStyle + `</style></head><body><main><h1>{{.Title}}</h1><p>{{.Message}}</p></main></body></html>`))

var errRender = errors.New("file preview: document could not be rendered")

func (s *Service) showLogin(w http.ResponseWriter, r *http.Request, next, message string, status int) {
	token, err := s.signedToken("csrf", csrfLifetime)
	if err != nil {
		writeStatus(w, r, http.StatusServiceUnavailable)
		return
	}
	var body bytes.Buffer
	if loginTemplate.Execute(&body, struct{ CSRF, Next, Error string }{token, next, message}) != nil {
		writeStatus(w, r, http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, s.cookie(csrfCookie, token, csrfLifetime))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body.Bytes())
	}
}

func writePage(w http.ResponseWriter, r *http.Request, status int, title, message string) {
	var body bytes.Buffer
	if messageTemplate.Execute(&body, struct{ Title, Message string }{title, message}) != nil {
		writeStatus(w, r, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body.Bytes())
	}
}

func renderPreview(file previewFile) ([]byte, error) {
	var rendered bytes.Buffer
	ext := strings.ToLower(filepath.Ext(file.name))
	isMarkdown := ext == ".md" || ext == ".markdown" || ext == ".mdown"
	isHTML := ext == ".html" || ext == ".htm" || ext == ".xhtml"
	if isMarkdown {
		md := goldmark.New(goldmark.WithExtensions(extension.GFM), goldmark.WithRendererOptions(
			renderer.WithNodeRenderers(util.Prioritized(&codeHighlighter{remaining: highlightBytes}, 100)),
		)) // Raw HTML and unsafe URL rendering stay disabled.
		document := md.Parser().Parse(text.NewReader(file.content))
		var images []*ast.Image
		var unsafeAutolinks []*ast.AutoLink
		var unsafeLinks []*ast.Link
		_ = ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
			if !entering {
				return ast.WalkContinue, nil
			}
			switch node := node.(type) {
			case *ast.Image:
				images = append(images, node)
				return ast.WalkSkipChildren, nil
			case *ast.AutoLink:
				if !safeRenderedLink(node.URL(file.content)) {
					unsafeAutolinks = append(unsafeAutolinks, node)
				}
			case *ast.Link:
				if !safeRenderedLink(node.Destination) {
					unsafeLinks = append(unsafeLinks, node)
				}
			}
			return ast.WalkContinue, nil
		})
		for _, image := range images {
			// Images are rendered as text; opening a local preview never fetches an
			// untrusted remote resource or forwards browser information to it.
			parent := image.Parent()
			parent.ReplaceChild(parent, image, ast.NewString([]byte("[图片："+string(image.Text(file.content))+"]")))
		}
		for _, link := range unsafeAutolinks {
			parent := link.Parent()
			parent.ReplaceChild(parent, link, ast.NewString(link.Label(file.content)))
		}
		for _, link := range unsafeLinks {
			parent := link.Parent()
			for child := link.FirstChild(); child != nil; {
				next := child.NextSibling()
				parent.InsertBefore(parent, link, child)
				child = next
			}
			parent.RemoveChild(parent, link)
		}
		if err := md.Renderer().Render(&rendered, file.content, document); err != nil {
			return nil, errRender
		}
	}
	var body bytes.Buffer
	var lexer chroma.Lexer
	language := ""
	if !isMarkdown {
		lexer = lexers.Match(file.name)
		if isHTML {
			lexer = lexers.Get("html")
		}
		if lexer != nil {
			language = lexer.Config().Name
		}
	}
	data := struct {
		Name, Modified, Language string
		// Keep HTMLDocument a string: html/template must escape the srcdoc
		// attribute. Its markup only belongs in the sandboxed child document.
		HTMLDocument       string
		IsMarkdown, IsHTML bool
		Markdown, Source   template.HTML
	}{Name: file.name, Modified: file.modified.UTC().Format(time.RFC3339), Language: language, IsMarkdown: isMarkdown, IsHTML: isHTML,
		Markdown: template.HTML(rendered.String()), Source: template.HTML(numberedCode(file.content, lexer))}
	if isHTML {
		data.HTMLDocument = staticHTMLDocument(file.content)
	}
	if previewTemplate.Execute(&body, data) != nil {
		return nil, errRender
	}
	return body.Bytes(), nil
}

func safeRenderedLink(destination []byte) bool {
	value := html.UnescapeString(string(util.UnescapePunctuations(destination)))
	for _, c := range value {
		if unicode.IsControl(c) {
			return false
		}
	}
	if strings.HasPrefix(value, "#") {
		return true
	}
	u, err := url.Parse(value)
	return err == nil && u.User == nil && (strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https")) && u.Host != ""
}

func numberedCode(content []byte, lexer chroma.Lexer) string {
	highlighter := codeHighlighter{remaining: highlightBytes}
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	lines := chroma.SplitTokensIntoLines(highlighter.tokens(text, lexer))
	if len(lines) == 0 {
		lines = [][]chroma.Token{{{Type: chroma.Text}}}
	}
	var source strings.Builder
	source.WriteString(`<pre class="source chroma"><code>`)
	for i, line := range lines {
		n := strconv.Itoa(i + 1)
		source.WriteString(`<span class="code-line" id="L` + n + `"><a class="line-no" href="#L` + n + `" aria-label="第 ` + n + ` 行">` + n + `</a>`)
		for j := range line {
			line[j].Value = strings.TrimSuffix(line[j].Value, "\n")
		}
		// A CR at the end of a line was also omitted by the plain source view.
		if len(line) > 0 {
			line[len(line)-1].Value = strings.TrimSuffix(line[len(line)-1].Value, "\r")
		}
		source.WriteString(tokenHTML(line))
		source.WriteString("</span>")
	}
	source.WriteString("</code></pre>")
	return source.String()
}
