package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"billing-wallet-service/internal/domain"

	"github.com/jackc/pgx/v5/pgconn"
)

// mapError marks failures worth retrying as domain.ErrRetryableContention or
// domain.ErrUnavailable. Every other error, including constraint violations, is
// returned unchanged.
func mapError(err error) error {
	if isContention(err) {
		return fmt.Errorf("%w: %w", domain.ErrRetryableContention, err)
	}

	if isUnavailable(err) {
		return fmt.Errorf("%w: %w", domain.ErrUnavailable, err)
	}

	return err
}

// isContention matches serialization failures (40001) and deadlocks (40P01).
func isContention(err error) bool {
	var pgErr *pgconn.PgError

	return errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01")
}

func isUnavailable(err error) bool {
	// A caller's cancellation or deadline also satisfies net.Error, but says nothing
	// about the database's health.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var connectErr *pgconn.ConnectError
	var netErr net.Error
	if errors.As(err, &connectErr) || errors.As(err, &netErr) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// 08: connection exception, 53: insufficient resources, 57P: server shutting down.
		for _, class := range []string{"08", "53", "57P"} {
			if strings.HasPrefix(pgErr.Code, class) {
				return true
			}
		}
	}

	return false
}
