package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"billing-wallet-service/internal/domain"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestMapErrorMarksContentionAsRetryable(t *testing.T) {
	for _, code := range []string{"40001", "40P01"} {
		err := &pgconn.PgError{Code: code}

		got := mapError(err)

		if !errors.Is(got, domain.ErrRetryableContention) {
			t.Errorf("code %s: %v is not ErrRetryableContention", code, got)
		}
		if errors.Is(got, domain.ErrUnavailable) {
			t.Errorf("code %s must not be reported as unavailable", code)
		}
	}
}

func TestMapError(t *testing.T) {
	tests := []struct {
		name            string
		err             error
		wantUnavailable bool
	}{
		{name: "network failure", err: fmt.Errorf("query: %w", &net.OpError{Op: "dial", Err: errors.New("refused")}), wantUnavailable: true},
		{name: "connection dropped mid query", err: fmt.Errorf("query: %w", io.ErrUnexpectedEOF), wantUnavailable: true},
		{name: "connection exception class", err: &pgconn.PgError{Code: "08006"}, wantUnavailable: true},
		{name: "server shutting down", err: &pgconn.PgError{Code: "57P01"}, wantUnavailable: true},
		{name: "too many connections", err: &pgconn.PgError{Code: "53300"}, wantUnavailable: true},
		{name: "constraint violation", err: &pgconn.PgError{Code: "23514"}},
		{name: "missing table", err: &pgconn.PgError{Code: "42P01"}},
		{name: "plain error", err: errors.New("boom")},
		{name: "deadline", err: context.DeadlineExceeded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapError(tt.err)

			if isUnavailable := errors.Is(got, domain.ErrUnavailable); isUnavailable != tt.wantUnavailable {
				t.Errorf("mapError(%v) = %v, unavailable = %v, want %v", tt.err, got, isUnavailable, tt.wantUnavailable)
			}
			if !errors.Is(got, tt.err) {
				t.Errorf("mapError(%v) = %v, lost the original error", tt.err, got)
			}
		})
	}
}
