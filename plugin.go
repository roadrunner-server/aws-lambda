package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	httpV1proto "github.com/roadrunner-server/api/v4/build/http/v1"
	"github.com/roadrunner-server/errors"
	"github.com/roadrunner-server/goridge/v3/pkg/frame"
	"github.com/roadrunner-server/pool/pool"
	"github.com/roadrunner-server/pool/worker"
	"google.golang.org/protobuf/proto"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/roadrunner-server/pool/payload"
	poolImp "github.com/roadrunner-server/pool/pool/static_pool"
	"go.uber.org/zap"
)

const (
	pluginName string = "lambda"
)

type Plugin struct {
	mu            sync.Mutex
	log           *zap.Logger
	srv           Server
	pldPool       sync.Pool
	wrkPool       Pool
	protoReqPool  sync.Pool
	protoRespPool sync.Pool
}

// Logger plugin
type Logger interface {
	NamedLogger(name string) *zap.Logger
}

type Pool interface {
	// Workers returns workers list associated with the pool.
	Workers() (workers []*worker.Process)
	// Exec payload
	Exec(ctx context.Context, p *payload.Payload, stopCh chan struct{}) (chan *poolImp.PExec, error)
	// RemoveWorker removes worker from the pool.
	RemoveWorker(ctx context.Context) error
	// AddWorker adds worker to the pool.
	AddWorker() error
	// Reset kill all workers inside the watcher and replaces with new
	Reset(ctx context.Context) error
	// Destroy all underlying stacks (but let them complete the task).
	Destroy(ctx context.Context)
}

// Server creates workers for the application.
type Server interface {
	NewPool(ctx context.Context, cfg *pool.Config, env map[string]string, _ *zap.Logger) (*poolImp.Pool, error)
}

func (p *Plugin) Init(srv Server, log Logger) error {
	p.srv = srv
	p.log = log.NamedLogger(pluginName)
	p.pldPool = sync.Pool{
		New: func() any {
			return &payload.Payload{
				Codec:   frame.CodecJSON,
				Context: make([]byte, 0, 100),
				Body:    make([]byte, 0, 100),
			}
		},
	}

	p.protoReqPool = sync.Pool{
		New: func() any {
			return &httpV1proto.Request{}
		},
	}
	p.protoRespPool = sync.Pool{
		New: func() any {
			return &httpV1proto.Response{}
		},
	}

	return nil
}

func (p *Plugin) Serve() chan error {
	errCh := make(chan error, 1)
	const op = errors.Op("plugin_serve")

	p.mu.Lock()
	defer p.mu.Unlock()

	var err error
	p.wrkPool, err = p.srv.NewPool(context.Background(), &pool.Config{
		NumWorkers:      4,
		AllocateTimeout: time.Second * 20,
		DestroyTimeout:  time.Second * 20,
	}, nil, nil)
	if err != nil {
		errCh <- errors.E(op, err)
		return errCh
	}

	go func() {
		// register handler
		lambda.Start(p.handler())
	}()

	return errCh
}

func (p *Plugin) Stop(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.wrkPool != nil {
		p.wrkPool.Destroy(ctx)
	}

	return nil
}

func (p *Plugin) handler() func(ctx context.Context, request events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
	return func(ctx context.Context, request events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
		reqProto := p.getProtoReq(request)
		defer p.putProtoReq(reqProto)

		pld := p.getPld()
		defer p.putPld(pld)
		cleanup := func() {}
		body := []byte(request.Body)
		if request.IsBase64Encoded {
			decoded, err := base64.StdEncoding.DecodeString(request.Body)
			if err != nil {
				return events.APIGatewayV2HTTPResponse{Body: err.Error(), StatusCode: 400}, nil
			}
			body = decoded
		}

		transformedBody, uploads, parsed, uploadsCleanup, err := p.transformBody(request, body)
		if err != nil {
			return events.APIGatewayV2HTTPResponse{Body: err.Error(), StatusCode: 400}, nil
		}
		if uploadsCleanup != nil {
			cleanup = uploadsCleanup
		}
		defer cleanup()

		reqProto.Parsed = parsed
		reqProto.Uploads = uploads

		rp, err := proto.Marshal(reqProto)
		if err != nil {
			return events.APIGatewayV2HTTPResponse{Body: err.Error(), StatusCode: 500}, nil
		}

		pld.Body = transformedBody
		pld.Context = rp

		re, err := p.wrkPool.Exec(ctx, pld, nil)
		if err != nil {
			return events.APIGatewayV2HTTPResponse{Body: err.Error(), StatusCode: 500}, nil
		}

		var r *payload.Payload

		select {
		case pl := <-re:
			if pl.Error() != nil {
				return events.APIGatewayV2HTTPResponse{Body: pl.Error().Error(), StatusCode: 500}, nil
			}
			// streaming is not supported
			if pl.Payload().Flags&frame.STREAM != 0 {
				return events.APIGatewayV2HTTPResponse{Body: "streaming is not supported", StatusCode: 500}, nil
			}

			// assign the payload
			r = pl.Payload()
		default:
			return events.APIGatewayV2HTTPResponse{Body: "worker empty response", StatusCode: 500}, nil
		}

		var response events.APIGatewayV2HTTPResponse
		err = p.handlePROTOresponse(r, &response)
		if err != nil {
			return events.APIGatewayV2HTTPResponse{Body: err.Error(), StatusCode: 500}, nil
		}

		return response, nil
	}
}

func (p *Plugin) putPld(pld *payload.Payload) {
	pld.Body = nil
	pld.Context = nil
	p.pldPool.Put(pld)
}

func (p *Plugin) getPld() *payload.Payload {
	pld := p.pldPool.Get().(*payload.Payload)
	pld.Codec = frame.CodecProto
	return pld
}

func (p *Plugin) putProtoRsp(rsp *httpV1proto.Response) {
	rsp.Headers = nil
	rsp.Status = -1
	p.protoRespPool.Put(rsp)
}

func (p *Plugin) getProtoRsp() *httpV1proto.Response {
	return p.protoRespPool.Get().(*httpV1proto.Response)
}

func (p *Plugin) getProtoReq(r events.APIGatewayV2HTTPRequest) *httpV1proto.Request {
	req := p.protoReqPool.Get().(*httpV1proto.Request)
	headers := normalizeHeaders(r)

	req.RemoteAddr = r.RequestContext.HTTP.SourceIP
	req.Protocol = r.RequestContext.HTTP.Protocol
	req.Method = r.RequestContext.HTTP.Method
	req.Uri = buildURI(r.RawPath, r.RawQueryString)
	req.Header = convert(headers)
	req.Cookies = convertCookies(r.Cookies, p.log)
	req.RawQuery = r.RawQueryString
	req.Parsed = false
	req.Attributes = make(map[string]*httpV1proto.HeaderValue)

	return req
}

func (p *Plugin) putProtoReq(req *httpV1proto.Request) {
	req.RemoteAddr = ""
	req.Protocol = ""
	req.Method = ""
	req.Uri = ""
	req.Header = nil
	req.Cookies = nil
	req.RawQuery = ""
	req.Parsed = false
	req.Uploads = nil
	req.Attributes = nil

	p.protoReqPool.Put(req)
}

func convertCookies(cookies []string, log *zap.Logger) map[string]*httpV1proto.HeaderValue {
	if len(cookies) == 0 {
		return nil
	}

	resp := make(map[string]*httpV1proto.HeaderValue, len(cookies))

	for _, h := range cookies {
		ck, err := http.ParseCookie(h)
		if err != nil {
			log.Error("failed to parse cookie", zap.Error(err))
			continue
		}

		for _, v := range ck {
			decoded, _ := url.QueryUnescape(v.Value)
			resp[v.Name] = &httpV1proto.HeaderValue{
				Value: [][]byte{[]byte(decoded)},
			}
		}
	}

	return resp
}

func convert(headers map[string]string) map[string]*httpV1proto.HeaderValue {
	if len(headers) == 0 {
		return nil
	}

	resp := make(map[string]*httpV1proto.HeaderValue, len(headers))

	for k, v := range headers {
		if resp[k] == nil {
			resp[k] = &httpV1proto.HeaderValue{}
		}

		resp[k].Value = append(resp[k].Value, []byte(v))
	}

	return resp
}

// normalizeHeaders guarantees that reverse-proxy headers exist and are
// consistent so Symfony can correctly detect the client and scheme when
// running behind CloudFront and API Gateway.
func normalizeHeaders(r events.APIGatewayV2HTTPRequest) map[string]string {
	headers := make(map[string]string, len(r.Headers)+6)
	for k, v := range r.Headers {
		if k == "" {
			continue
		}
		headers[strings.ToLower(k)] = v
	}

	sourceIP := strings.TrimSpace(r.RequestContext.HTTP.SourceIP)
	if sourceIP != "" {
		if existing, ok := headers["x-forwarded-for"]; ok && existing != "" {
			if !strings.Contains(existing, sourceIP) {
				headers["x-forwarded-for"] = existing + ", " + sourceIP
			}
		} else {
			headers["x-forwarded-for"] = sourceIP
		}
	}

	proto := headers["x-forwarded-proto"]
	if proto == "" {
		switch {
		case headers["cloudfront-forwarded-proto"] != "":
			proto = headers["cloudfront-forwarded-proto"]
		case headers["x-amzn-scheme"] != "":
			proto = headers["x-amzn-scheme"]
		case strings.HasPrefix(strings.ToLower(r.RequestContext.DomainName), "localhost"):
			proto = "http"
		default:
			proto = "https"
		}
		headers["x-forwarded-proto"] = proto
	}

	host := headers["x-forwarded-host"]
	if host == "" {
		// Prioritize the actual Host header from CloudFront over API Gateway domain
		switch {
		case headers["host"] != "":
			host = headers["host"]
		case r.RequestContext.DomainName != "":
			host = r.RequestContext.DomainName
		}
		if host != "" {
			headers["x-forwarded-host"] = host
		}
	}

	if _, ok := headers["x-forwarded-port"]; !ok || headers["x-forwarded-port"] == "" {
		if strings.EqualFold(proto, "https") {
			headers["x-forwarded-port"] = "443"
		} else {
			headers["x-forwarded-port"] = "80"
		}
	}

	if _, ok := headers["x-forwarded-prefix"]; !ok {
		stage := strings.TrimSpace(r.RequestContext.Stage)
		if stage != "" && stage != "$default" && stage != "default" {
			if !strings.HasPrefix(stage, "/") {
				stage = "/" + stage
			}
			headers["x-forwarded-prefix"] = stage
		}
	}

	if _, ok := headers["forwarded"]; !ok && sourceIP != "" && host != "" {
		headers["forwarded"] = "for=" + sourceIP + ";proto=" + proto + ";host=" + host
	}

	return headers
}

func (p *Plugin) handlePROTOresponse(pld *payload.Payload, response *events.APIGatewayV2HTTPResponse) error {
	rsp := p.getProtoRsp()
	defer p.putProtoRsp(rsp)
	response.Headers = make(map[string]string)

	if len(pld.Context) != 0 {
		// unmarshal context into response
		err := proto.Unmarshal(pld.Context, rsp)
		if err != nil {
			return err
		}

		// write all headers from the response to the writer
		for k := range rsp.GetHeaders() {
			for kk := range rsp.GetHeaders()[k].GetValue() {
				response.Headers[k] = string(rsp.GetHeaders()[k].GetValue()[kk])
			}
		}

		response.StatusCode = int(rsp.Status)
	}

	// do not write body if it is empty
	if len(pld.Body) == 0 {
		return nil
	}

	response.Body = string(pld.Body)

	return nil
}

func (p *Plugin) transformBody(request events.APIGatewayV2HTTPRequest, body []byte) ([]byte, []byte, bool, func(), error) {
	ct := strings.ToLower(request.Headers["content-type"])
	switch contentType(request.RequestContext.HTTP.Method, ct) {
	case contentNone:
		return nil, nil, false, nil, nil
	case contentURLEncoded:
		b, err := parseURLEncoded(body, request.Headers)
		if err != nil {
			return nil, nil, false, nil, err
		}
		return b, nil, true, nil, nil
	case contentMultipart:
		b, uploads, err := parseMultipart(body, request.Headers)
		if err != nil {
			return nil, nil, false, nil, err
		}

		var uploadsBytes []byte
		if uploads != nil {
			uploadsBytes, err = json.Marshal(uploads)
			if err != nil {
				return nil, nil, false, nil, err
			}
		}

		cleanup := func() {
			if uploads != nil {
				uploads.Clear()
			}
		}

		return b, uploadsBytes, true, cleanup, nil
	default:
		return body, nil, false, nil, nil
	}
}

func buildURI(path, rawQuery string) string {
	if path == "" {
		path = "/"
	}
	if rawQuery == "" {
		return path
	}
	return path + "?" + rawQuery
}
