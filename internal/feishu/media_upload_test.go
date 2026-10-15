package feishu

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestUploadImageMultipart(t *testing.T) {
	data := []byte{0, 1, 2, 0xff, '\n'}
	var uploads atomic.Int32
	g := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			w.Write([]byte(`{"code":0,"tenant_access_token":"fixture-token","expire":7200}`))
			return
		}
		uploads.Add(1)
		if r.Method != "POST" || r.URL.Path != "/open-apis/im/v1/images" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Error("unexpected image upload request")
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer r.MultipartForm.RemoveAll()
		if len(r.MultipartForm.Value) != 1 || r.FormValue("image_type") != "message" || len(r.MultipartForm.File) != 1 {
			t.Error("unexpected multipart fields")
		}
		file, header, err := r.FormFile("image")
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer file.Close()
		actual, err := io.ReadAll(file)
		if err != nil || header.Filename != "附件.png" || !bytes.Equal(actual, data) {
			t.Error("image bytes or safe basename changed")
		}
		w.Write([]byte(`{"code":0,"data":{"image_key":"img_fixture"}}`))
	})
	key, err := g.UploadImage(context.Background(), "/private/source/附件.png", data)
	if err != nil || key != "img_fixture" || uploads.Load() != 1 {
		t.Fatalf("upload result key=%q err=%v uploads=%d", key, err, uploads.Load())
	}
}

func TestUploadImageUnknownNeverRetries(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		disconnect bool
	}{
		{name: "missing_key", body: `{"code":0,"data":{}}`},
		{name: "empty_key", body: `{"code":0,"data":{"image_key":""}}`},
		{name: "invalid_response", body: `not json`},
		{name: "server_error", body: `{"code":0,"data":{"image_key":"unconfirmed"}}`, status: 500},
		{name: "lost_response", disconnect: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var uploads atomic.Int32
			g := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(r.URL.Path, "tenant_access_token") {
					w.Write([]byte(`{"code":0,"tenant_access_token":"fixture-token","expire":7200}`))
					return
				}
				uploads.Add(1)
				if tc.disconnect {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
					return
				}
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				w.Write([]byte(tc.body))
			})
			key, err := g.UploadImage(context.Background(), "fixture.png", []byte("fixture"))
			var unknown *Unknown
			if key != "" || !errors.As(err, &unknown) || uploads.Load() != 1 {
				t.Fatalf("unsafe upload result key=%q err=%v uploads=%d", key, err, uploads.Load())
			}
		})
	}
}

func TestUploadImagePermissionFailure(t *testing.T) {
	var uploads atomic.Int32
	g := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			w.Write([]byte(`{"code":0,"tenant_access_token":"fixture-token","expire":7200}`))
			return
		}
		uploads.Add(1)
		w.Header().Set("X-Tt-Logid", "upload-request-fixture")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"code":99991672,"msg":"long-secret-never-log","error":{"permission_violations":[{"subject":"im:resource:upload"}]},"data":{"image_key":"rejected"}}`))
	})
	key, err := g.UploadImage(context.Background(), "fixture.png", []byte("fixture"))
	var rejected *Rejected
	if key != "" || !errors.As(err, &rejected) || rejected.Code != 99991672 || rejected.RequestID != "upload-request-fixture" || len(rejected.RequiredPermissions) != 1 || rejected.RequiredPermissions[0] != "im:resource:upload" || strings.Contains(err.Error(), g.secret) || uploads.Load() != 1 {
		t.Fatalf("unsafe permission failure key=%q err=%v uploads=%d", key, err, uploads.Load())
	}
}

func TestUploadImageInvalidInputNeverRequests(t *testing.T) {
	var requests atomic.Int32
	g := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{name: "fixture.png"},
		{name: "fixture.png", data: make([]byte, maxUploadImageBytes+1)},
		{name: "unsafe\r\n.png", data: []byte("fixture")},
		{name: "unsafe\x00.png", data: []byte("fixture")},
		{name: "unsafe\t.png", data: []byte("fixture")},
		{name: "unsafe\u0085.png", data: []byte("fixture")},
		{name: "/private\n/fixture.png", data: []byte("fixture")},
		{name: "", data: []byte("fixture")},
		{name: "/", data: []byte("fixture")},
	} {
		key, err := g.UploadImage(context.Background(), tc.name, tc.data)
		if key != "" || err == nil {
			t.Error("invalid upload input accepted")
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid input reached transport: requests=%d", requests.Load())
	}
}
