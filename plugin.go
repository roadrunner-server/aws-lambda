package main

import (
	"context"
	"net/http"
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
		rp, err := proto.Marshal(reqProto)
		if err != nil {
			return events.APIGatewayV2HTTPResponse{Body: err.Error(), StatusCode: 500}, nil
		}

		pld.Body = []byte(request.Body)
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
		rsp := p.getProtoRsp()
		defer p.putProtoRsp(rsp)

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

	req.RemoteAddr = r.RequestContext.HTTP.SourceIP
	req.Protocol = r.RequestContext.HTTP.Protocol
	req.Method = r.RequestContext.HTTP.Method
	req.Uri = r.RawPath
	req.Header = convert(r.Headers)
	req.Cookies = convertCookies(r.Cookies, p.log)
	req.RawQuery = r.RawQueryString
	req.Parsed = true
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
			resp[v.Name] = &httpV1proto.HeaderValue{
				Value: [][]byte{[]byte(v.Value)},
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

func (p *Plugin) handlePROTOresponse(pld *payload.Payload, response *events.APIGatewayV2HTTPResponse) error {
	rsp := p.getProtoRsp()
	defer p.putProtoRsp(rsp)

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
