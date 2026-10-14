# aws-lambda

A RoadRunner plugin that runs the server inside an AWS Lambda custom runtime.

Lambda leaves no place for a long-lived process: a container is alive only while it handles an
invocation, and the runtime has to ask for work itself instead of listening on a port. This plugin
serves the Lambda Runtime API and hands every invocation to the plugins that implement
`Invokable`, so a plugin only has to say how it starts and stops its work for the duration of one
invocation.

```go
type Invokable interface {
    StartInvocation(ctx context.Context, graceful time.Duration) error
    StopInvocation(ctx context.Context) error
    Name() string
}
```

Plugins never import this one: endure matches the methods, the same way the `resetter` plugin
collects `Resetter`.

## Configuration

The plugin disables itself unless `AWS_LAMBDA_RUNTIME_API` is present, so the same binary behaves
normally outside Lambda. The section is optional.

```yaml
lambda:
  # Reserved before the invocation deadline so that every handler can stop and the Runtime API
  # answer still fits. Must exceed graceful_timeout.
  shutdown_buffer: 6s
  # How long a handler may drain its in-flight work.
  graceful_timeout: 5s
```

Set the Lambda function timeout well above the buffer, otherwise an invocation ends before the
handlers have done any work.

## Building a binary with this plugin

The official RoadRunner build does not contain it, so build one with
[velox](https://github.com/roadrunner-server/velox) and list the plugins the function actually
needs — a Temporal worker on Lambda comes out at 28MB instead of 59MB:

```toml
[roadrunner]
ref = "master"

[target_platform]
os = "linux"
arch = "arm64"

[plugins.lambda]
tag = "master"
module_name = "github.com/roadrunner-server/aws-lambda"

[plugins.temporal]
tag = "latest"
module_name = "github.com/temporalio/roadrunner-temporal/v6"

# logger, server and rpc omitted for brevity
```

```bash
vx build -c velox.toml -o .
```

Velox renders `container/plugins.go` itself, so nothing has to be registered by hand.

## Example: a Temporal worker

`roadrunner-temporal` implements `Invokable` by cycling only its Temporal workers, while the PHP
worker pools stay up for the whole lifetime of the execution environment — so no PHP process is
restarted between invocations. The function image runs the binary built above and needs no PHP
entrypoint of its own:

```dockerfile
COPY rr /usr/local/bin/rr
ENTRYPOINT ["rr", "serve"]
```

A worked example, including the Dockerfile, the `velox.toml` and the Runtime Interface Emulator
setup, lives in [temporalio/samples-php](https://github.com/temporalio/samples-php).

## examples/http

`examples/http` is the previous content of this repository: a standalone binary that turns an
API Gateway v2 event into an HTTP request for a PHP worker. It is kept as is, since it covers the
other Lambda shape — one event, one response — which the `Invokable` interface above does not.
Folding it into a handler of this plugin is the obvious next step.
