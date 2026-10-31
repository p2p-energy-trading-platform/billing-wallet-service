package grpc

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func UnaryServerLogger(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		start := time.Now()
		resp, err := handler(ctx, req)

		duration := time.Since(start)
		st, _ := status.FromError(err)
		code := st.Code()

		logFn := logger.Info
		msg := "gRPC request completed"

		if code != codes.OK {
			switch code {
			case codes.InvalidArgument, codes.NotFound, codes.AlreadyExists, codes.Unauthenticated, codes.PermissionDenied:
				// Client-side errors
				logFn = logger.Warn
				msg = "gRPC client error"
			default:
				// Internal, Unknown
				logFn = logger.Error
				msg = "gRPC server error"
			}
		}

		attrs := []any{
			"method", info.FullMethod,
			"duration_ms", duration.Milliseconds(),
			"code", code.String(),
		}
		if err != nil {
			attrs = append(attrs, "error", err)
		}

		logFn(msg, attrs...)

		return resp, err
	}
}

func UnaryServerRecovery(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (resp interface{}, err error) {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic recovered in gRPC server",
					"method", info.FullMethod,
					"panic", r,
				)
				err = status.Error(codes.Internal, "internal server error")
			}
		}()

		return handler(ctx, req)
	}
}
