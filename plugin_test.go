package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"unsafe"

	"github.com/aws/aws-lambda-go/events"
	httpV1proto "github.com/roadrunner-server/api/v4/build/http/v1"
	"github.com/roadrunner-server/pool/payload"
	poolImp "github.com/roadrunner-server/pool/pool/static_pool"
	"github.com/roadrunner-server/pool/worker"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

type namedLoggerStub struct{}

func (namedLoggerStub) NamedLogger(name string) *zap.Logger {
	return zap.NewNop()
}

type fakePool struct {
	mu              sync.Mutex
	requests        []*httpV1proto.Request
	bodies          [][]byte
	uploads         [][]byte
	responseBody    string
	responseHeaders map[string]string
	responseStatus  int64
}

func newFakePool() *fakePool {
	return &fakePool{
		responseBody:    "ok",
		responseHeaders: map[string]string{"Content-Type": "text/plain"},
		responseStatus:  200,
	}
}

func (fp *fakePool) Workers() (workers []*worker.Process) {
	return nil
}

func (fp *fakePool) Exec(ctx context.Context, pld *payload.Payload, stopCh chan struct{}) (chan *poolImp.PExec, error) {
	fp.mu.Lock()
	defer fp.mu.Unlock()

	req := &httpV1proto.Request{}
	_ = proto.Unmarshal(pld.Context, req)

	fp.requests = append(fp.requests, req)
	fp.bodies = append(fp.bodies, append([]byte(nil), pld.Body...))
	fp.uploads = append(fp.uploads, append([]byte(nil), req.Uploads...))

	rsp := &httpV1proto.Response{
		Status: fp.responseStatus,
		Headers: map[string]*httpV1proto.HeaderValue{
			"Content-Type": {Value: [][]byte{[]byte(fp.responseHeaders["Content-Type"])}},
		},
	}

	respCtx, _ := proto.Marshal(rsp)
	responsePayload := &payload.Payload{
		Body:    []byte(fp.responseBody),
		Context: respCtx,
	}

	ch := make(chan *poolImp.PExec, 1)
	ch <- buildPExec(responsePayload, nil)

	return ch, nil
}

func (fp *fakePool) RemoveWorker(ctx context.Context) error { return nil }
func (fp *fakePool) AddWorker() error                       { return nil }
func (fp *fakePool) Reset(ctx context.Context) error        { return nil }
func (fp *fakePool) Destroy(ctx context.Context)            {}

func buildPExec(pld *payload.Payload, err error) *poolImp.PExec {
	pe := &poolImp.PExec{}
	setUnexportedField(pe, "pld", pld)
	setUnexportedField(pe, "err", err)
	return pe
}

func setUnexportedField(target any, name string, value any) {
	v := reflect.ValueOf(target).Elem().FieldByName(name)
	if !reflect.ValueOf(value).IsValid() {
		reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().Set(reflect.Zero(v.Type()))
		return
	}

	reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().Set(reflect.ValueOf(value))
}

func TestHandlerBuildsLambdaRequests(t *testing.T) {
	tests := []struct {
		name            string
		method          string
		path            string
		rawQuery        string
		contentType     string
		body            string
		cookies         []string
		isBase64        bool
		expectedBody    string
		hasExpectedBody bool
		expectUploads   bool
	}{
		{
			name:        "getWithQuery",
			method:      http.MethodGet,
			path:        "/status/check",
			rawQuery:    "q=spaces+and+plus&emoji=%F0%9F%98%8A",
			contentType: "text/html",
			body:        "",
		},
		{
			name:        "postJSON",
			method:      http.MethodPost,
			path:        "/api/resource",
			rawQuery:    "foo=bar",
			contentType: "application/json",
			body:        `{"name":"roadrunner","active":true}`,
		},
		{
			name:            "putFormURLEncoded",
			method:          http.MethodPut,
			path:            "/submit/form",
			rawQuery:        "",
			contentType:     "application/x-www-form-urlencoded",
			body:            "full_name=Ada+Lovelace&priority=high",
			expectedBody:    `{"full_name":"Ada Lovelace","priority":"high"}`,
			hasExpectedBody: true,
		},
		{
			name:            "postFormURLEncodedBase64",
			method:          http.MethodPost,
			path:            "/submit/form",
			rawQuery:        "",
			contentType:     "application/x-www-form-urlencoded",
			body:            "ZnVsbF9uYW1lPUFkYStMb3ZlbGFjZSZwcm9qZWN0PWxsYW1iZGE=", // base64: full_name=Ada+Lovelace&project=llambda
			expectedBody:    `{"full_name":"Ada Lovelace","project":"llambda"}`,
			hasExpectedBody: true,
			isBase64:        true,
		},
		{
			name:            "deleteMultipart",
			method:          http.MethodDelete,
			path:            "/upload/image",
			rawQuery:        "version=1",
			contentType:     "multipart/form-data; boundary=----demo",
			body:            "------demo\r\nContent-Disposition: form-data; name=\"file\"; filename=\"demo.png\"\r\nContent-Type: image/png\r\n\r\nPNGDATA\r\n------demo--",
			expectedBody:    `{}`,
			hasExpectedBody: true,
			expectUploads:   true,
		},
		{
			name:            "postMultipartBase64",
			method:          http.MethodPost,
			path:            "/upload/image",
			rawQuery:        "",
			contentType:     "multipart/form-data; boundary=----demo",
			body:            "LS0tLS0tZGVtbw0KQ29udGVudC1EaXNwb3NpdGlvbjogZm9ybS1kYXRhOyBuYW1lPSJmaWxlIjsgZmlsZW5hbWU9ImRlbW8ucG5nIg0KQ29udGVudC1UeXBlOiBpbWFnZS9wbmcNCg0KUE5HREFUQQ0KLS0tLS0tZGVtby0t", // base64 of multipart payload
			expectedBody:    `{}`,
			hasExpectedBody: true,
			isBase64:        true,
			expectUploads:   true,
		},
		{
			name:            "optionsOctetBase64",
			method:          http.MethodOptions,
			path:            "/raw",
			rawQuery:        "",
			contentType:     "application/octet-stream",
			body:            "AAECAw==", // base64 of 0x00 0x01 0x02 0x03
			expectedBody:    "",
			hasExpectedBody: true,
			isBase64:        true,
		},
		{
			name:            "pngBodyBase64",
			method:          http.MethodPost,
			path:            "/images",
			rawQuery:        "",
			contentType:     "image/png",
			body:            "iVBORw0K", // base64 of first bytes 0x89PNG\r\n
			expectedBody:    string([]byte{0x89, 'P', 'N', 'G', '\r', '\n'}),
			hasExpectedBody: true,
			isBase64:        true,
		},
		{
			name:        "patchText",
			method:      http.MethodPatch,
			path:        "/notes/123",
			rawQuery:    "",
			contentType: "text/plain",
			body:        "patched text body",
		},
		{
			name:            "headXML",
			method:          http.MethodHead,
			path:            "/xml/feed",
			rawQuery:        "page=2",
			contentType:     "application/xml",
			body:            "<feed><id>1</id></feed>",
			expectedBody:    "",
			hasExpectedBody: true,
		},
		{
			name:            "optionsOctet",
			method:          http.MethodOptions,
			path:            "/raw",
			rawQuery:        "",
			contentType:     "application/octet-stream",
			body:            string([]byte{0x00, 0x01, 0x02, 0x03}),
			expectedBody:    "",
			hasExpectedBody: true,
		},
		{
			name:        "graphql",
			method:      http.MethodPost,
			path:        "/graphql",
			rawQuery:    "",
			contentType: "application/graphql",
			body:        "{ viewer { login } }",
			cookies:     []string{"session=abc123; theme=light"},
		},
		{
			name:        "htmlBody",
			method:      http.MethodPost,
			path:        "/page",
			rawQuery:    "preview=true",
			contentType: "text/html",
			body:        "<html><body>demo</body></html>",
		},
		{
			name:        "pngBody",
			method:      http.MethodPost,
			path:        "/images",
			rawQuery:    "",
			contentType: "image/png",
			body:        string([]byte{0x89, 'P', 'N', 'G'}),
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			p := &Plugin{}
			if err := p.Init(nil, namedLoggerStub{}); err != nil {
				t.Fatalf("init error: %v", err)
			}

			fp := newFakePool()
			p.wrkPool = fp

			handler := p.handler()
			req := events.APIGatewayV2HTTPRequest{
				Headers: map[string]string{},
				RequestContext: events.APIGatewayV2HTTPRequestContext{
					HTTP: events.APIGatewayV2HTTPRequestContextHTTPDescription{
						SourceIP: "203.0.113.1",
						Method:   tt.method,
						Path:     tt.path,
						Protocol: "HTTP/1.1",
					},
				},
				RawPath:        tt.path,
				RawQueryString: tt.rawQuery,
				Body:           tt.body,
				Cookies:        tt.cookies,
			}

			if tt.isBase64 {
				req.IsBase64Encoded = true
			}

			if tt.contentType != "" {
				req.Headers["content-type"] = tt.contentType
			}

			response, err := handler(context.Background(), req)
			if err != nil {
				t.Fatalf("handler error: %v", err)
			}

			if response.StatusCode != int(fp.responseStatus) {
				t.Fatalf("unexpected status code: %d", response.StatusCode)
			}

			if len(fp.requests) != 1 {
				t.Fatalf("expected one captured request, got %d", len(fp.requests))
			}

			gotReq := fp.requests[0]

			if gotReq.Method != tt.method {
				t.Errorf("method mismatch: got %s want %s", gotReq.Method, tt.method)
			}

			expectedURI := buildURI(tt.path, tt.rawQuery)
			if gotReq.Uri != expectedURI {
				t.Errorf("uri mismatch: got %s want %s", gotReq.Uri, expectedURI)
			}

			if gotReq.RawQuery != tt.rawQuery {
				t.Errorf("raw query mismatch: got %s want %s", gotReq.RawQuery, tt.rawQuery)
			}

			if tt.contentType != "" {
				header := gotReq.Header["content-type"]
				if header == nil || len(header.Value) == 0 {
					t.Fatalf("expected content-type header")
				}
				if got := string(header.Value[0]); got != tt.contentType {
					t.Errorf("content-type mismatch: got %s want %s", got, tt.contentType)
				}
			}

			if len(fp.bodies) != 1 {
				t.Fatalf("expected one captured body, got %d", len(fp.bodies))
			}

			expectedBody := tt.body
			if tt.hasExpectedBody {
				expectedBody = tt.expectedBody
			}

			gotBody := string(fp.bodies[0])
			if json.Valid([]byte(expectedBody)) {
				var gotJSON map[string]any
				var wantJSON map[string]any
				if err := json.Unmarshal([]byte(gotBody), &gotJSON); err != nil {
					t.Fatalf("body not valid json: %v", err)
				}
				if err := json.Unmarshal([]byte(expectedBody), &wantJSON); err != nil {
					t.Fatalf("expected body invalid json: %v", err)
				}
				if !reflect.DeepEqual(gotJSON, wantJSON) {
					t.Errorf("json body mismatch: got %v want %v", gotJSON, wantJSON)
				}
			} else if gotBody != expectedBody {
				t.Errorf("body mismatch: got %q want %q", gotBody, expectedBody)
			}

			if tt.expectUploads {
				if len(fp.uploads) != 1 || len(fp.uploads[0]) == 0 {
					t.Fatalf("expected uploads metadata to be present")
				}
				if !bytes.Contains(fp.uploads[0], []byte("demo.png")) {
					t.Fatalf("expected upload metadata to contain filename, got %s", string(fp.uploads[0]))
				}
			}

			if len(tt.cookies) > 0 {
				if gotReq.Cookies == nil || len(gotReq.Cookies) == 0 {
					t.Fatalf("expected cookies to be parsed")
				}
			}
		})
	}
}

func TestHandleProtoResponse(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(nil, namedLoggerStub{}); err != nil {
		t.Fatalf("init error: %v", err)
	}

	rspProto := &httpV1proto.Response{
		Status: 202,
		Headers: map[string]*httpV1proto.HeaderValue{
			"Content-Type": {Value: [][]byte{[]byte("application/json")}},
			"X-Test":       {Value: [][]byte{[]byte("value")}},
		},
	}

	ctxBytes, _ := proto.Marshal(rspProto)
	pld := &payload.Payload{
		Body:    []byte(`{"ok":true}`),
		Context: ctxBytes,
	}

	var response events.APIGatewayV2HTTPResponse
	if err := p.handlePROTOresponse(pld, &response); err != nil {
		t.Fatalf("handle response error: %v", err)
	}

	if response.StatusCode != 202 {
		t.Fatalf("status code mismatch: got %d want %d", response.StatusCode, 202)
	}

	if response.Headers["Content-Type"] != "application/json" {
		t.Fatalf("content type mismatch: got %s", response.Headers["Content-Type"])
	}

	if response.Body != `{"ok":true}` {
		t.Fatalf("body mismatch: got %s", response.Body)
	}
}
