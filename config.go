package awslambda

import (
	"time"

	"github.com/roadrunner-server/errors"
)

// responseReserve is kept aside for the Runtime API response after the handlers stop.
const responseReserve = time.Second

type Config struct {
	// ShutdownBuffer is reserved before the deadline for stopping the handlers
	// and answering the Runtime API.
	ShutdownBuffer time.Duration `mapstructure:"shutdown_buffer"`
	// GracefulTimeout is how long a handler may drain its in-flight work.
	GracefulTimeout time.Duration `mapstructure:"graceful_timeout"`
}

func (c *Config) InitDefaults() error {
	const op = errors.Op("lambda_config_init_defaults")

	if c.GracefulTimeout == 0 {
		c.GracefulTimeout = time.Second * 5
	}

	if c.ShutdownBuffer == 0 {
		c.ShutdownBuffer = c.GracefulTimeout + responseReserve
	}

	if c.ShutdownBuffer <= c.GracefulTimeout {
		return errors.E(op, errors.Errorf(
			"lambda.shutdown_buffer (%s) must exceed lambda.graceful_timeout (%s) to leave room for the Runtime API response",
			c.ShutdownBuffer, c.GracefulTimeout,
		))
	}

	return nil
}
