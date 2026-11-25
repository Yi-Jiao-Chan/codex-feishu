package filepreview

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strconv"
	"unicode/utf8"
)

const artifactPageBytes = 64 << 10

const artifactPreviewStyle = `
.file-tools{display:flex;flex-wrap:wrap;align-items:center;justify-content:space-between;gap:12px;margin:16px 0}
.file-tools nav,.file-tools form{display:flex;flex-wrap:wrap;align-items:center;gap:10px}
.file-tools input{box-sizing:border-box;width:7ch;padding:6px;border:1px solid #d8dde7;border-radius:4px;font:inherit}
.file-tools button{font-size:inherit;padding:6px 12px}
.file-tools a,.file-tools button{min-height:40px;display:inline-flex;align-items:center}
.file-page{white-space:pre-wrap;overflow-wrap:anywhere;max-height:65vh;margin:0;tab-size:4}
.file-progress{display:block;width:min(100%,32rem);height:1rem;accent-color:#1d4ed8}
`

var artifactPreviewTemplate = template.Must(template.New("artifact-preview").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Name}} · {{.Kind}}</title><style>` + pageStyle + artifactPreviewStyle + `</style></head>
<body><main><header><h1>{{.Name}}</h1><p class="meta">{{.Kind}} · {{.Size}}</p></header>
<div class="file-tools"><nav aria-label="文件分页">
{{if .Prev}}<a href="?page={{.Prev}}">上一页</a>{{end}}
<span>第 {{.Page}} / {{.Pages}} 页</span>
{{if .Next}}<a href="?page={{.Next}}">下一页</a>{{end}}
</nav><form method="get"><label for="file-page-number">页码</label>
<input id="file-page-number" name="page" type="number" inputmode="numeric" min="1" max="{{.Pages}}" value="{{.Page}}" required>
<button type="submit">跳转</button></form></div>
<p class="meta">字节 {{.Start}}–{{.End}}</p>
<pre class="file-page" tabindex="0" aria-label="当前页内容"><code>{{.Text}}</code></pre>
</main></body></html>`))

var artifactPreparationTemplate = template.Must(template.New("artifact-preparation").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>正在准备文件</title><style>` + pageStyle + artifactPreviewStyle + `</style></head>
<body><main><h1>正在准备文件</h1>
<progress class="file-progress" aria-label="文件准备进度" value="{{.Copied}}" max="{{.Total}}"></progress>
<p>{{.Percent}}% · {{.Transferred}} / {{.Size}}</p>
<p class="meta">{{if .Publishing}}正在完成校验并发布。{{else}}准备完成后自动打开预览。{{end}}</p>
</main></body></html>`))

var largeDocumentNoticeTemplate = template.Must(template.New("large-document").Parse(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Name}} · 文件预览</title><style>` + pageStyle + `</style></head><body><main><h1>{{.Name}}</h1><p>此文件超过 {{.Limit}} 的直接渲染上限。</p><a href="{{.SourcePath}}">查看源码</a></main></body></html>`))

func serveLargeDocumentNotice(w http.ResponseWriter, r *http.Request, name, sourcePath string, limit int64) {
	data := struct{ Name, SourcePath, Limit string }{name, sourcePath, artifactSizeLabel(limit)}
	writeArtifactTemplate(w, r, http.StatusOK, largeDocumentNoticeTemplate, data)
}

func parseArtifactPageNumber(value string) (int64, bool) {
	page, err := strconv.ParseInt(value, 10, 64)
	return page, err == nil && page >= 1 && page <= maxArtifactBytes/artifactPageBytes &&
		strconv.FormatInt(page, 10) == value
}

func serveArtifactPreview(w http.ResponseWriter, r *http.Request, f io.ReaderAt, name string, size int64) {
	pages := (size + artifactPageBytes - 1) / artifactPageBytes
	page := int64(1)
	if values, exists := r.URL.Query()["page"]; exists {
		var ok bool
		if len(values) != 1 {
			writeStatus(w, r, http.StatusBadRequest)
			return
		}
		if page, ok = parseArtifactPageNumber(values[0]); !ok || page > pages {
			writeStatus(w, r, http.StatusBadRequest)
			return
		}
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	text, from, to, err := readArtifactPage(f, size, page)
	if err != nil {
		writeStatus(w, r, http.StatusServiceUnavailable)
		return
	}
	data := struct {
		Name, Kind, Size, Text              string
		Page, Pages, Prev, Next, Start, End int64
	}{
		Name: name, Kind: "文本预览", Size: artifactSizeLabel(size), Text: text,
		Page: page, Pages: pages, Start: from + 1, End: to,
	}
	if isHTMLFile(name) {
		data.Kind = "HTML 源码预览"
	} else if isMarkdownFile(name) {
		data.Kind = "Markdown 源码预览"
	}
	if page > 1 {
		data.Prev = page - 1
	}
	if page < pages {
		data.Next = page + 1
	}
	writeArtifactTemplate(w, r, http.StatusOK, artifactPreviewTemplate, data)
}

// Place a split rune on the following page. The small overlap lets adjacent
// pages find the same boundary without splitting a character or dropping bytes.
func readArtifactPage(f io.ReaderAt, size, page int64) (string, int64, int64, error) {
	start := (page - 1) * artifactPageBytes
	end := min(size, start+artifactPageBytes)
	readFrom := max(int64(0), start-utf8.UTFMax+1)
	data := make([]byte, min(size, end+1)-readFrom)
	if _, err := f.ReadAt(data, readFrom); err != nil {
		return "", 0, 0, err
	}
	from, to := start-readFrom, end-readFrom
	for from > 0 && !utf8.RuneStart(data[from]) {
		from--
	}
	for to < int64(len(data)) && to > from && !utf8.RuneStart(data[to]) {
		to--
	}
	if !utf8.Valid(data[from:to]) {
		return "", 0, 0, errNotPreviewable
	}
	return string(data[from:to]), readFrom + from, readFrom + to, nil
}

func serveArtifactPreparation(w http.ResponseWriter, r *http.Request, progress artifactPreparation) {
	total := max(progress.size, 1)
	copied := min(progress.copied, total)
	data := struct {
		Copied, Total, Percent int64
		Transferred, Size      string
		Publishing             bool
	}{
		Copied: copied, Total: total, Percent: copied * 100 / total,
		Transferred: artifactSizeLabel(copied), Size: artifactSizeLabel(progress.size),
		Publishing: copied == total,
	}
	writeArtifactTemplate(w, r, http.StatusAccepted, artifactPreparationTemplate, data)
}

func artifactSizeLabel(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d 字节", n)
	}
}

func writeArtifactTemplate(w http.ResponseWriter, r *http.Request, status int, t *template.Template, data any) {
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	var body bytes.Buffer
	if err := t.Execute(&body, data); err != nil {
		writeStatus(w, r, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(body.Bytes())
}
