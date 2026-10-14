// Package awslambda runs RoadRunner inside an AWS Lambda custom runtime: it
// serves the Lambda Runtime API and hands every invocation to the plugins that
// implement Invokable.
package awslambda

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/roadrunner-server/endure/v2/dep"
	"github.com/roadrunner-server/errors"
)

const (
	pluginName     = "lambda"
	runtimeAPIEnv  = "AWS_LAMBDA_RUNTIME_API"
	minimumRunTime = time.Second
)

// Invokable is implemented by plugins that do their work per Lambda invocation
// instead of running forever. The plugin is never imported by them: endure
// matches the methods, the same way the resetter plugin collects Resetter.
type Invokable interface {
	// StartInvocation begins the work for one invocation, within the graceful
	// budget the handler will be given to drain.
	StartInvocation(ctx context.Context, graceful time.Duration) error
	// StopInvocation drains it before the deadline.
	StopInvocation(ctx context.Context) error
	Name() string
}

type Configurer interface {
	UnmarshalKey(name string, out any) error
	Has(name string) bool
}

type Logger interface {
	NamedLogger(name string) *slog.Logger
}

type Plugin struct {
	log      *slog.Logger
	cfg      *Config
	registry map[string]Invokable
}

func (p *Plugin) Init(cfg Configurer, log Logger) error {
	const op = errors.Op("lambda_plugin_init")

	if os.Getenv(runtimeAPIEnv) == "" {
		return errors.E(errors.Disabled)
	}

	p.cfg = &Config{}
	if cfg.Has(pluginName) {
		if err := cfg.UnmarshalKey(pluginName, p.cfg); err != nil {
			return errors.E(op, err)
		}
	}

	if err := p.cfg.InitDefaults(); err != nil {
		return errors.E(op, err)
	}

	p.log = log.NamedLogger(pluginName)
	p.registry = make(map[string]Invokable)

	return nil
}

func (p *Plugin) Collects() []*dep.In {
	return []*dep.In{
		dep.Fits(func(pl any) {
			invokable := pl.(Invokable)
			p.registry[invokable.Name()] = invokable
		}, (*Invokable)(nil)),
	}
}

func (p *Plugin) Serve() chan error {
	errCh := make(chan error, 1)

	p.log.Info("serving the lambda runtime api",
		"shutdown_buffer", p.cfg.ShutdownBuffer.String(),
		"graceful_timeout", p.cfg.GracefulTimeout.String(),
		"handlers", len(p.registry),
	)

	go func() {
		lambda.Start(p.handle)
	}()

	return errCh
}

func (p *Plugin) Stop(context.Context) error {
	return nil
}

func (p *Plugin) Name() string {
	return pluginName
}

func (p *Plugin) handle(ctx context.Context) error {
	const op = errors.Op("lambda_invocation")

	deadline, ok := ctx.Deadline()
	if !ok {
		return errors.E(op, errors.Str("the Runtime API did not provide an invocation deadline"))
	}

	stopAt := deadline.Add(-p.cfg.ShutdownBuffer)

	runFor := time.Until(stopAt)
	if runFor < minimumRunTime {
		return errors.E(op, errors.Errorf(
			"insufficient invocation time: %s left after reserving a %s shutdown buffer",
			runFor, p.cfg.ShutdownBuffer,
		))
	}

	started := p.startAll(ctx)
	p.log.Info("invocation started", "polling_for", runFor.String(), "handlers", len(started))

	p.waitUntil(ctx, stopAt)
	p.stopAll(started)

	p.log.Info("invocation finished", "remaining", time.Until(deadline).String())

	return nil
}

func (p *Plugin) startAll(ctx context.Context) []Invokable {
	started := make([]Invokable, 0, len(p.registry))

	for name, invokable := range p.registry {
		if err := invokable.StartInvocation(ctx, p.cfg.GracefulTimeout); err != nil {
			p.log.Error("handler failed to start", "handler", name, "error", err)
			continue
		}

		started = append(started, invokable)
	}

	return started
}

func (p *Plugin) stopAll(started []Invokable) {
	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.GracefulTimeout)
	defer cancel()

	for i := range started {
		if err := started[i].StopInvocation(ctx); err != nil {
			p.log.Error("handler failed to stop", "handler", started[i].Name(), "error", err)
		}
	}
}

func (p *Plugin) waitUntil(ctx context.Context, stopAt time.Time) {
	timer := time.NewTimer(time.Until(stopAt))
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}
