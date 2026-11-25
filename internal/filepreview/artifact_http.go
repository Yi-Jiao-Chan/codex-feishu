package filepreview

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
)

func (s *Service) serveArtifact(w http.ResponseWriter, r *http.Request, cache *artifactCache, id, relative string, source *os.File, info fs.FileInfo, action string) {
	if cache == nil {
		_ = source.Close()
		writeStatus(w, r, http.StatusServiceUnavailable)
		return
	}
	if action == "html" && info.Size() > s.maxBytes {
		_ = source.Close()
		writeStatus(w, r, http.StatusNotFound)
		return
	}
	version := s.artifactVersion(id, info)
	f, release, err := cache.request(s, id, version, source, info.Size(), r.Method == http.MethodGet)
	if err != nil {
		artifactStatus(w, r, err, action != "html")
		return
	}
	defer release()
	name := filepath.Base(relative)
	base := "/f/" + id
	if info.Size() > s.maxBytes {
		if action == "" && (isHTMLFile(name) || isMarkdownFile(name)) {
			if r.URL.RawQuery != "" {
				http.Redirect(w, r, base+"/source?"+r.URL.RawQuery, http.StatusSeeOther)
			} else {
				serveLargeDocumentNotice(w, r, name, base+"/source", s.maxBytes)
			}
			return
		}
		serveArtifactPreview(w, r, f, name, info.Size())
		return
	}
	if action == "" && isHTMLFile(name) {
		s.serveRendered(w, r, previewFile{name: name, modified: info.ModTime()}, base, action)
		return
	}
	body, err := io.ReadAll(io.LimitReader(f, s.maxBytes+1))
	if err != nil || int64(len(body)) != info.Size() {
		writeStatus(w, r, http.StatusServiceUnavailable)
		return
	}
	s.serveRendered(w, r, previewFile{name: name, modified: info.ModTime(), content: body}, base, action)
}

func artifactStatus(w http.ResponseWriter, r *http.Request, err error, refresh bool) {
	switch {
	case errors.Is(err, errArtifactPending):
		w.Header().Set("Retry-After", "2")
		if !refresh {
			writeStatus(w, r, http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodGet {
			w.Header().Set("Refresh", "2")
		}
		var progress artifactPreparation
		_ = errors.As(err, &progress)
		serveArtifactPreparation(w, r, progress)
	case errors.Is(err, errArtifactQuota):
		writePage(w, r, http.StatusInsufficientStorage, "文件缓存空间不足", "请待过期成品清理后重试。")
	case errors.Is(err, errNotPreviewable):
		writeStatus(w, r, http.StatusNotFound)
	default:
		w.Header().Set("Retry-After", "15")
		writePage(w, r, http.StatusServiceUnavailable, "暂时无法准备文件", "请稍后重试。")
	}
}
