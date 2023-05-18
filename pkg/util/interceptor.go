package util

import (
	"context"
	"time"

	"github.com/bufbuild/connect-go"
	"github.com/go-kit/log"
)

type timeoutInterceptor struct {
	timeout time.Duration
}

// NewTimeoutInterceptor returns a new timeout interceptor.
func WithTimeout(timeout time.Duration) connect.Interceptor {
	return timeoutInterceptor{timeout: timeout}
}

func (s timeoutInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return connect.UnaryFunc(func(ctx context.Context, ar connect.AnyRequest) (connect.AnyResponse, error) {
		ctx, cancel := context.WithTimeout(ctx, s.timeout)
		defer cancel()
		return next(ctx, ar)
	})
}

func (s timeoutInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return connect.StreamingClientFunc(func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		ctx, cancel := context.WithTimeout(ctx, s.timeout)
		defer cancel()
		return next(ctx, spec)
	})
}

func (s timeoutInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return connect.StreamingHandlerFunc(func(ctx context.Context, shc connect.StreamingHandlerConn) error {
		ctx, cancel := context.WithTimeout(ctx, s.timeout)
		defer cancel()
		return next(ctx, shc)
	})
}

// LogRequest logs the request parameters.
// It logs all kinds of requests.
func NewLogInterceptor(logger log.Logger) connect.UnaryInterceptorFunc {
	interceptor := func(next connect.UnaryFunc) connect.UnaryFunc {
		return connect.UnaryFunc(func(
			ctx context.Context,
			req connect.AnyRequest,
		) (connect.AnyResponse, error) {
			logger.Log(
				"msg", "request parameters",
				"route", req.Spec().Procedure,
				"parameters", req.Any(),
			)
			return next(ctx, req)
		})
	}
	return connect.UnaryInterceptorFunc(interceptor)
}
