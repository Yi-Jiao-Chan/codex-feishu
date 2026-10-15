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

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

const pageStyle = `body{margin:0;color:#20252b;background:#f6f7f9;font:16px/1.65 system-ui,-apple-system,sans-serif}main{max-width:1040px;margin:24px auto;padding:24px;background:white;border:1px solid #e1e5eb;border-radius:10px}h1{font-size:1.4rem;overflow-wrap:anywhere}.meta{font-size:.85rem;color:#606a78}a{color:#1765bb}pre{overflow:auto;background:#f6f8fa;padding:14px;border-radius:6px;line-height:1.55}code{font-family:ui-monospace,SFMono-Regular,Consolas,monospace;font-size:.88rem}.code-line{display:block;min-height:1.55em;scroll-margin-top:16px}.code-line:target{background:#fff1aa}.line-no{display:inline-block;min-width:3.5em;margin-right:1.25em;text-align:right;color:#77818d;text-decoration:none;user-select:none}table{border-collapse:collapse;max-width:100%;display:block;overflow:auto}td,th{padding:5px 10px;border:1px solid #d8dee7}blockquote{margin-left:0;padding-left:14px;border-left:3px solid #d8dee7;color:#56616f}input[type=password]{display:block;box-sizing:border-box;width:100%;max-width:420px;padding:10px;font:inherit;margin:8px 0 16px}button{padding:9px 22px;background:#1765bb;border:0;border-radius:6px;color:white;font:inherit;cursor:pointer}.error{color:#a42121}.login{max-width:480px;margin:64px auto}details{margin-top:24px}summary{cursor:pointer}@media(max-width:650px){main{margin:0;padding:16px;border-radius:0;border:0}.login{margin:24px auto}pre{margin-left:-8px;margin-right:-8px;padding:10px}.line-no{min-width:2.5em;margin-right:.75em}}`

var loginTemplate = template.Must(template.New("login").Parse(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>文件预览登录</title><style>` + pageStyle + `</style></head><body><main class="login"><h1>文件预览</h1><p>输入预览口令后打开已分享的文件。</p>{{if .Error}}<p class="error" role="alert">{{.Error}}</p>{{end}}<form method="post" action=""><input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="next" value="{{.Next}}"><label for="password">预览口令</label><input type="password" id="password" name="password" autocomplete="current-password" maxlength="256" required autofocus><button type="submit">打开预览</button></form></main></body></html>`))

var previewTemplate = template.Must(template.New("preview").Parse(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Name}} · 文件预览</title><style>` + pageStyle + `</style></head><body><main><h1>{{.Name}}</h1><p class="meta">当前文件内容 · 修改时间 {{.Modified}} · 每次打开读取最新内容</p>{{if .Markdown}}<article>{{.Markdown}}</article><details><summary>查看带行号源文</summary>{{.Source}}</details>{{else}}{{.Source}}{{end}}</main></body></html>`))

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
	if ext == ".md" || ext == ".markdown" || ext == ".mdown" {
		md := goldmark.New(goldmark.WithExtensions(extension.GFM)) // Raw HTML and unsafe URL rendering stay disabled.
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
	data := struct {
		Name, Modified   string
		Markdown, Source template.HTML
	}{Name: file.name, Modified: file.modified.UTC().Format(time.RFC3339),
		Markdown: template.HTML(rendered.String()), Source: template.HTML(numberedSource(file.content))}
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

func numberedSource(content []byte) string {
	lines := strings.Split(string(content), "\n")
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var source strings.Builder
	source.WriteString("<pre><code>")
	for i, line := range lines {
		n := strconv.Itoa(i + 1)
		source.WriteString(`<span class="code-line" id="L` + n + `"><a class="line-no" href="#L` + n + `" aria-label="第 ` + n + ` 行">` + n + `</a>`)
		source.WriteString(html.EscapeString(strings.TrimSuffix(line, "\r")))
		source.WriteString("</span>")
	}
	source.WriteString("</code></pre>")
	return source.String()
}
