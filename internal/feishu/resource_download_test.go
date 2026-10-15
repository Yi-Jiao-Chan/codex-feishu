package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/larksuite/oapi-sdk-go/v3/core"
)

func resourceTestGateway(t *testing.T, handler http.HandlerFunc) (*Gateway, *httptest.Server, *atomic.Int32) {
	t.Helper()
	var authCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == core.TenantAccessTokenInternalUrlPath {
			authCalls.Add(1)
			var body map[string]string
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["app_id"] != "test_"+t.Name() || body["app_secret"] != "test-secret" {
				t.Error("wrong application token identity")
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"code":0,"expire":7200,"tenant_access_token":"test-tenant-token"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-tenant-token" {
			t.Error("missing application tenant token")
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	g := New("test_"+t.Name(), "test-secret")
	g.cfg.BaseUrl = server.URL
	g.cfg.HttpClient = server.Client()
	return g, server, &authCalls
}

func TestOpenMessageResourceStreamsOriginalMessageWithTenantIdentity(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	g, _, authCalls := resourceTestGateway(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/open-apis/im/v1/messages/om_original/resources/img_v3_key" || r.URL.RawQuery != "type=image" {
			t.Errorf("wrong original reference: %s", r.URL.RequestURI())
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", "9")
		w.Header().Set("Content-Disposition", `attachment; filename*=UTF-8''%E4%B8%AD%E6%96%87.png`)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-release:
			fmt.Fprint(w, "PNG-bytes")
		case <-r.Context().Done():
		}
	})
	type result struct {
		body     io.ReadCloser
		metadata ResourceMetadata
		err      error
	}
	opened := make(chan result, 1)
	go func() {
		body, metadata, err := g.OpenMessageResource(context.Background(), "om_original", "img_v3_key", "image")
		opened <- result{body, metadata, err}
	}()
	select {
	case got := <-opened:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.metadata.ContentType != "image/png" || got.metadata.FileName != "中文.png" || got.metadata.ContentLength != 9 {
			t.Fatalf("metadata = %#v", got.metadata)
		}
		if err := got.body.Close(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Open buffered binary body instead of returning the response stream")
	}
	if calls.Load() != 1 || authCalls.Load() != 1 {
		t.Fatalf("resource/auth calls = %d/%d", calls.Load(), authCalls.Load())
	}
}

func TestOpenMessageResourceMetadataAndUnknownLength(t *testing.T) {
	g, _, authCalls := resourceTestGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="数据.json"`)
		w.(http.Flusher).Flush()
		fmt.Fprint(w, `{"code":234003,"actual_file":true}`)
	})
	for range 2 {
		body, metadata, err := g.OpenMessageResource(context.Background(), "om_original", "file_v3_key", "file")
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(body)
		_ = body.Close()
		if err != nil || string(data) != `{"code":234003,"actual_file":true}` || metadata.ContentLength != -1 || metadata.FileName != "数据.json" {
			t.Fatalf("body=%q metadata=%#v err=%v", data, metadata, err)
		}
	}
	if authCalls.Load() != 1 {
		t.Fatalf("did not reuse SDK token cache: %d", authCalls.Load())
	}
}

func TestOpenMessageResourceJSONErrorsAndHTTPFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		wantCode int
	}{
		{"denied", 403, `{"code":234040,"msg":"private-body test-secret img_v3_key"}`, 234040},
		{"error200", 200, `{"code":234003,"msg":"private-body"}`, 234003},
		{"malformed", 503, "private-body", 0},
		{"bounded", 400, `{"code":234003,"msg":"` + strings.Repeat("x", maxResourceErrorBody) + `"}`, 0},
		{"partial", 206, "partial-bytes", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			g, _, _ := resourceTestGateway(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Tt-Logid", "safe-request-123")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			body, _, err := g.OpenMessageResource(context.Background(), "om_original", "img_v3_key", "image")
			var rejected *Rejected
			if body != nil || !errors.As(err, &rejected) || rejected.Code != tc.wantCode || rejected.HTTPStatus != tc.status || rejected.RequestID != "safe-request-123" {
				t.Fatalf("result body=%v err=%#v", body, err)
			}
			for _, secret := range []string{"private-body", "test-secret", "img_v3_key", "test-tenant-token"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("unsafe error: %v", err)
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("GET retried: %d", calls.Load())
			}
		})
	}
}

func TestOpenMessageResourceRejectsRedirectWithoutChangingSharedClient(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); fmt.Fprint(w, "bad") }))
	defer target.Close()
	g, _, _ := resourceTestGateway(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) })
	configured := g.cfg.HttpClient.(*http.Client)
	_, _, err := g.OpenMessageResource(context.Background(), "om_original", "file_key", "file")
	var rejected *Rejected
	if !errors.As(err, &rejected) || rejected.HTTPStatus != 302 || redirected.Load() != 0 {
		t.Fatalf("redirect: %v, followed=%d", err, redirected.Load())
	}
	if configured.CheckRedirect != nil {
		t.Fatal("mutated shared SDK client")
	}
}

func TestOpenMessageResourceCancellationAndDeadlineCoverBody(t *testing.T) {
	g, _, _ := resourceTestGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	g.cfg.ReqTimeout = 100 * time.Millisecond
	body, _, err := g.OpenMessageResource(context.Background(), "om_original", "img_key", "image")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(body); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected body deadline error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("body outlived resource deadline")
	}
}

func TestOpenMessageResourceRejectsInjectedReferencesBeforeHTTP(t *testing.T) {
	var calls atomic.Int32
	g, _, authCalls := resourceTestGateway(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	for _, tc := range [][3]string{{"../message", "file_key", "file"}, {"om_message", "file/key", "file"}, {"om_message", "key?type=image", "file"}, {"om_message", "key", "video"}, {"om_message", "https://example.invalid/a", "image"}} {
		if body, _, err := g.OpenMessageResource(context.Background(), tc[0], tc[1], tc[2]); err == nil || body != nil {
			t.Fatalf("accepted %v", tc)
		}
	}
	if calls.Load() != 0 || authCalls.Load() != 0 {
		t.Fatal("invalid references made network requests")
	}
}

type resourceRoundTripFunc func(*http.Request) (*http.Response, error)

func (f resourceRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type countedResourceBody struct {
	remaining int
	read      int
	closed    bool
	failure   error
}

func (b *countedResourceBody) Read(p []byte) (int, error) {
	if b.failure != nil {
		return 0, b.failure
	}
	if b.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), b.remaining)
	for i := range n {
		p[i] = 'x'
	}
	b.remaining -= n
	b.read += n
	return n, nil
}
func (b *countedResourceBody) Close() error { b.closed = true; return b.failure }

func TestOpenMessageResourceBoundsErrorReadsAndHidesTransportFailures(t *testing.T) {
	for _, mode := range []string{"large-error", "transport-error", "body-error"} {
		t.Run(mode, func(t *testing.T) {
			g := New("test_"+t.Name(), "test-secret")
			var calls int
			body := &countedResourceBody{remaining: 10 * maxResourceErrorBody}
			g.cfg.HttpClient = &http.Client{Transport: resourceRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == core.TenantAccessTokenInternalUrlPath {
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"code":0,"expire":7200,"tenant_access_token":"test-tenant-token"}`))}, nil
				}
				calls++
				if mode == "transport-error" {
					return nil, errors.New("private-body img_key test-tenant-token")
				}
				status := 400
				if mode == "body-error" {
					status = 200
					body.failure = errors.New("private-body img_key test-tenant-token")
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/octet-stream"}, "X-Tt-Logid": {"test-tenant-token"}}, Body: body}, nil
			})}
			stream, _, err := g.OpenMessageResource(context.Background(), "om_message", "img_key", "image")
			if mode == "body-error" {
				if err != nil {
					t.Fatal(err)
				}
				_, err = io.ReadAll(stream)
				if closeErr := stream.Close(); closeErr != nil && strings.Contains(closeErr.Error(), "private-body") {
					t.Fatal("unsafe close error")
				}
			}
			if err == nil {
				t.Fatal("expected failure")
			}
			for _, secret := range []string{"private-body", "img_key", "test-tenant-token"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("unsafe error: %v", err)
				}
			}
			if calls != 1 {
				t.Fatalf("retried resource GET %d times", calls)
			}
			if mode == "large-error" && (!body.closed || body.read > maxResourceErrorBody+1) {
				t.Fatalf("error read = %d, closed=%v", body.read, body.closed)
			}
		})
	}
}

func TestOpenMessageResourcePreservesJSONWithoutFilename(t *testing.T) {
	data := `{"code":234003,"description":"` + strings.Repeat("x", 2*maxResourceErrorBody) + `"}`
	g, _, _ := resourceTestGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, data)
	})
	body, _, err := g.OpenMessageResource(context.Background(), "om_message", "file_key", "file")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil || string(got) != data {
		t.Fatalf("JSON body changed: got=%d want=%d err=%v", len(got), len(data), err)
	}
}

func TestOpenMessageResourceJSONCodeFieldIsNotAnAPIEnvelope(t *testing.T) {
	data := `{"code":234003,"actual_file":true}`
	g, _, _ := resourceTestGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, data)
	})
	body, _, err := g.OpenMessageResource(context.Background(), "om_message", "file_key", "file")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil || string(got) != data {
		t.Fatalf("JSON body changed: %q, %v", got, err)
	}
}

func TestOpenMessageResourceTruncatedContentLengthFailsDuringRead(t *testing.T) {
	g, _, _ := resourceTestGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", "100")
		fmt.Fprint(w, "short")
	})
	body, metadata, err := g.OpenMessageResource(context.Background(), "om_message", "img_key", "image")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err == nil || string(data) != "short" || metadata.ContentLength != 100 {
		t.Fatalf("truncation data=%q length=%d err=%v", data, metadata.ContentLength, err)
	}
}
