package main

import (
	"context"
	"sync"
	"time"

	"github.com/goccy/go-json"
	"github.com/roadrunner-server/errors"
	"github.com/roadrunner-server/goridge/v3/pkg/frame"
	"github.com/roadrunner-server/sdk/v4/pool"
	"github.com/roadrunner-server/sdk/v4/worker"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/roadrunner-server/sdk/v4/payload"
	poolImp "github.com/roadrunner-server/sdk/v4/pool/static_pool"
	"go.uber.org/zap"
)

const (
	pluginName string = "lambda"
)

type Plugin struct {
	mu      sync.Mutex
	log     *zap.Logger
	srv     Server
	pldPool sync.Pool
	wrkPool Pool
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
		requestJSON, err := json.Marshal(request)
		if err != nil {
			return events.APIGatewayV2HTTPResponse{Body: "", StatusCode: 500}, nil
		}

		ctxJSON, err := json.Marshal(ctx)
		if err != nil {
			return events.APIGatewayV2HTTPResponse{Body: "", StatusCode: 500}, nil
		}

		pld := p.getPld()
		defer p.putPld(pld)

		pld.Body = requestJSON
		pld.Context = ctxJSON

		re, err := p.wrkPool.Exec(ctx, pld, nil)
		if err != nil {
			return events.APIGatewayV2HTTPResponse{Body: "", StatusCode: 500}, nil
		}

		var r *payload.Payload

		select {
		case pl := <-re:
			if pl.Error() != nil {
				return events.APIGatewayV2HTTPResponse{Body: "", StatusCode: 500}, nil
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

		if err != nil {
			return events.APIGatewayV2HTTPResponse{Body: "", StatusCode: 500}, nil
		}

		var response events.APIGatewayV2HTTPResponse
		err = json.Unmarshal(r.Body, &response)
		if err != nil {
			return events.APIGatewayV2HTTPResponse{Body: "", StatusCode: 500}, nil
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
	return pld
}
