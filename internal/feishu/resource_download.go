package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	core "github.com/larksuite/oapi-sdk-go/v3/core"
)

type ResourceMetadata struct {
	ContentType   string
	FileName      string
	ContentLength int64
}

const maxResourceErrorBody = 64 << 10

// resourceRequestCapture lets the SDK construct an authenticated request using
// its existing tenant-token cache. Its resource response is synthetic: the real
// GET happens below, outside core.Request's unconditional response buffering.
// Token requests still use the same controlled HTTP client and SDK identity.
type resourceRequestCapture struct {
	client  *http.Client
	url     string
	request *http.Request
}

func (c *resourceRequestCapture) Do(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodGet && req.URL.String() == c.url {
		c.request = req.Clone(req.Context()) // Clone also snapshots headers.
		body := `{"code":0}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: req}, nil
	}
	return c.client.Do(req)
}

// OpenMessageResource opens one original message's binary resource. The caller
// owns Close and must enforce byte/type limits while reading. It never retries
// the resource GET; retry policy belongs to the persisted download worker.
func (g *Gateway) OpenMessageResource(ctx context.Context, messageID, key, kind string) (io.ReadCloser, ResourceMetadata, error) {
	var metadata ResourceMetadata
	if !validResourceIdentifier(messageID) || !validResourceIdentifier(key) || kind != "image" && kind != "file" {
		return nil, metadata, errors.New("invalid Feishu message resource reference")
	}
	if g == nil || g.cfg == nil {
		return nil, metadata, errors.New("Feishu resource client unavailable")
	}
	// Copy the client, never mutate the shared SDK client's redirect policy.
	client := &http.Client{}
	if g.cfg.HttpClient != nil {
		configured, ok := g.cfg.HttpClient.(*http.Client)
		if !ok {
			return nil, metadata, errors.New("Feishu resource client requires redirect control")
		}
		*client = *configured
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	timeout := g.cfg.ReqTimeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	streamCtx, cancel := context.WithTimeout(ctx, timeout)
	fail := func(err error) (io.ReadCloser, ResourceMetadata, error) {
		cancel()
		return nil, ResourceMetadata{}, err
	}
	path := "/open-apis/im/v1/messages/" + messageID + "/resources/" + key
	fullURL := strings.TrimRight(g.cfg.BaseUrl, "/") + path + "?type=" + kind
	capture := &resourceRequestCapture{client: client, url: fullURL}
	config := *g.cfg
	config.BaseUrl = strings.TrimRight(config.BaseUrl, "/")
	config.HttpClient = capture
	config.Logger = discardLogger{}
	config.LogReqAtDebug = false
	_, err := core.Request(streamCtx, &core.ApiReq{HttpMethod: http.MethodGet, ApiPath: path, QueryParams: core.QueryParams{"type": {kind}}, SupportedAccessTokenTypes: []core.AccessTokenType{core.AccessTokenTypeTenant}}, &config)
	if err != nil || capture.request == nil {
		return fail(&Unknown{Operation: "authenticate message resource download"})
	}
	resp, err := client.Do(capture.request)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return fail(&Unknown{Operation: "download message resource"})
	}
	if resp == nil || resp.Body == nil {
		return fail(&Unknown{Operation: "download message resource"})
	}
	requestID := safeResourceRequestID(resp.Header, key, g.secret, strings.TrimPrefix(capture.request.Header.Get("Authorization"), "Bearer "))
	reject := func(code int) (io.ReadCloser, ResourceMetadata, error) {
		_ = resp.Body.Close()
		return fail(&Rejected{Code: code, HTTPStatus: resp.StatusCode, RequestID: requestID})
	}
	if resp.StatusCode != http.StatusOK {
		// No Range was requested, so a partial response cannot be a complete asset.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResourceErrorBody+1))
		code := 0
		if len(body) <= maxResourceErrorBody {
			code = resourceErrorCode(body)
		}
		return reject(code)
	}
	metadata = ResourceMetadata{ContentType: resp.Header.Get("Content-Type"), ContentLength: resp.ContentLength}
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		metadata.FileName = params["filename"]
	}
	reader := io.Reader(resp.Body)
	mediaType, _, _ := mime.ParseMediaType(metadata.ContentType)
	if (mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")) && metadata.FileName == "" {
		// Feishu can return a JSON API error with HTTP 200. Inspect only a bounded
		// prefix; a legitimate JSON file is returned with every original byte.
		prefix, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResourceErrorBody+1))
		if readErr != nil {
			_ = resp.Body.Close()
			return fail(&Unknown{Operation: "read message resource response"})
		}
		if len(prefix) <= maxResourceErrorBody {
			var envelope struct {
				Message *string `json:"msg"`
			}
			// A generic JSON document may have its own numeric code field. A
			// successful HTTP response needs the Feishu envelope's msg field too.
			if code := resourceErrorCode(prefix); code != 0 && json.Unmarshal(prefix, &envelope) == nil && envelope.Message != nil {
				return reject(code)
			}
		}
		reader = io.MultiReader(bytes.NewReader(prefix), resp.Body)
	}
	return &resourceStream{Reader: reader, body: resp.Body, cancel: cancel}, metadata, nil
}

func resourceErrorCode(body []byte) int {
	var envelope struct {
		Code *int `json:"code"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Code != nil {
		return *envelope.Code
	}
	return 0
}

func safeResourceRequestID(headers http.Header, secrets ...string) string {
	id := headers.Get(core.HttpHeaderKeyLogId)
	if id == "" {
		id = headers.Get(core.HttpHeaderKeyRequestId)
	}
	if len(id) > 128 {
		return ""
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_.:-", c)) {
			return ""
		}
	}
	for _, secret := range secrets {
		if secret != "" && strings.Contains(id, secret) {
			return ""
		}
	}
	return id
}

type resourceStream struct {
	io.Reader
	body     io.ReadCloser
	cancel   context.CancelFunc
	once     sync.Once
	closeErr error
}

func (r *resourceStream) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err != nil {
		_ = r.Close()
		if err != io.EOF {
			err = &Unknown{Operation: "read message resource stream"}
		}
	}
	return n, err
}

func (r *resourceStream) Close() error {
	r.once.Do(func() {
		r.cancel()
		if r.body.Close() != nil {
			r.closeErr = &Unknown{Operation: "close message resource stream"}
		}
	})
	return r.closeErr
}
