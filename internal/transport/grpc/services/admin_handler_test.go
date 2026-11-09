package services

import (
	"context"
	"testing"
	"time"

	"billing-wallet-service/internal/domain"

	"github.com/google/uuid"
	billingv1 "github.com/p2p-energy-trading-platform/go-sdk/gen/gridx/billing/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeFeeConfigUsecase struct {
	config  domain.FeeConfig
	err     error
	request domain.SetFeeConfigRequest
	calls   int
}

func (f *fakeFeeConfigUsecase) SetFeeConfig(_ context.Context, request domain.SetFeeConfigRequest) (domain.FeeConfig, error) {
	f.calls++
	f.request = request
	return f.config, f.err
}

func adminContext(actor, role string) context.Context {
	md := metadata.MD{}
	if actor != "" {
		md.Append(userIDMetadataKey, actor)
	}
	if role != "" {
		md.Append(userRoleMetadataKey, role)
	}
	return metadata.NewIncomingContext(context.Background(), md)
}

func validSetFeeConfig() *billingv1.SetFeeConfigRequest {
	return &billingv1.SetFeeConfigRequest{
		IdempotencyKey: "fee-change-1",
		BuyerFeeBps:    proto.Int32(150),
		SellerFeeBps:   proto.Int32(0),
	}
}

func TestSetFeeConfigMapsTheRequestAndResponse(t *testing.T) {
	actor, configID := uuid.New(), uuid.New()
	effective := time.Date(2026, 12, 1, 10, 0, 0, 123456789, time.UTC)
	created := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	usecase := &fakeFeeConfigUsecase{config: domain.FeeConfig{
		ID: configID, BuyerFeeBps: 150, SellerFeeBps: 0,
		EffectiveFrom: effective, CreatedBy: &actor, CreatedAt: created,
	}}
	request := validSetFeeConfig()
	request.EffectiveFrom = timestamppb.New(effective)

	response, err := NewAdminHandler(usecase, discardLogger()).SetFeeConfig(adminContext(actor.String(), "admin"), request)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := usecase.request
	if got.IdempotencyKey != "fee-change-1" || got.BuyerFeeBps != 150 || got.SellerFeeBps != 0 || got.ActorID != actor {
		t.Errorf("request = %+v", got)
	}
	// Postgres keeps microseconds, so a retry must compare equal to the stored value.
	if got.EffectiveFrom == nil || !got.EffectiveFrom.Equal(effective.Truncate(time.Microsecond)) {
		t.Errorf("EffectiveFrom = %v, want %v", got.EffectiveFrom, effective.Truncate(time.Microsecond))
	}
	config := response.GetFeeConfig()
	if config.GetFeeConfigId() != configID.String() || config.GetBuyerFeeBps() != 150 || config.GetSellerFeeBps() != 0 ||
		config.GetCreatedBy() != actor.String() || !config.GetEffectiveFrom().AsTime().Equal(effective) || !config.GetCreatedAt().AsTime().Equal(created) {
		t.Errorf("config = %v", config)
	}
}

func TestSetFeeConfigLeavesStartTimeUnsetWhenNotGiven(t *testing.T) {
	usecase := &fakeFeeConfigUsecase{}

	_, err := NewAdminHandler(usecase, discardLogger()).SetFeeConfig(adminContext(uuid.NewString(), "admin"), validSetFeeConfig())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usecase.request.EffectiveFrom != nil {
		t.Errorf("EffectiveFrom = %v, want nil", usecase.request.EffectiveFrom)
	}
}

func TestSetFeeConfigAcceptsTheRoleInAnyCase(t *testing.T) {
	for _, role := range []string{"admin", "ADMIN", "Admin"} {
		usecase := &fakeFeeConfigUsecase{}

		_, err := NewAdminHandler(usecase, discardLogger()).SetFeeConfig(adminContext(uuid.NewString(), role), validSetFeeConfig())

		if err != nil {
			t.Errorf("role %q: unexpected error: %v", role, err)
		}
	}
}

func TestSetFeeConfigRejectsCallersWhoAreNotAdmins(t *testing.T) {
	tests := []struct {
		name     string
		ctx      context.Context
		wantCode codes.Code
	}{
		{"consumer", adminContext(uuid.NewString(), "CONSUMER"), codes.PermissionDenied},
		{"no role", adminContext(uuid.NewString(), ""), codes.PermissionDenied},
		{"no actor", adminContext("", "admin"), codes.Unauthenticated},
		{"no metadata", context.Background(), codes.PermissionDenied},
		{"actor not a uuid", adminContext("nope", "admin"), codes.InvalidArgument},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usecase := &fakeFeeConfigUsecase{}

			_, err := NewAdminHandler(usecase, discardLogger()).SetFeeConfig(tt.ctx, validSetFeeConfig())

			if code := status.Code(err); code != tt.wantCode {
				t.Errorf("code = %v (%v), want %v", code, err, tt.wantCode)
			}
			if usecase.calls != 0 {
				t.Error("usecase must not be called")
			}
		})
	}
}

// 0 is a valid rate, so a missing field must not quietly become a free side.
func TestSetFeeConfigRequiresBothRates(t *testing.T) {
	tests := map[string]*billingv1.SetFeeConfigRequest{
		"no buyer rate":  {IdempotencyKey: "k", SellerFeeBps: proto.Int32(100)},
		"no seller rate": {IdempotencyKey: "k", BuyerFeeBps: proto.Int32(100)},
	}

	for name, request := range tests {
		t.Run(name, func(t *testing.T) {
			usecase := &fakeFeeConfigUsecase{}

			_, err := NewAdminHandler(usecase, discardLogger()).SetFeeConfig(adminContext(uuid.NewString(), "admin"), request)

			if code := status.Code(err); code != codes.InvalidArgument {
				t.Errorf("code = %v, want InvalidArgument", code)
			}
			if usecase.calls != 0 {
				t.Error("usecase must not be called")
			}
		})
	}
}

func TestSetFeeConfigMapsUsecaseErrors(t *testing.T) {
	usecase := &fakeFeeConfigUsecase{err: domain.ErrIdempotencyConflict}

	_, err := NewAdminHandler(usecase, discardLogger()).SetFeeConfig(adminContext(uuid.NewString(), "admin"), validSetFeeConfig())

	if code := status.Code(err); code != codes.AlreadyExists {
		t.Errorf("code = %v, want AlreadyExists", code)
	}
}

func TestSetFeeConfigRejectsAnOutOfRangeStartTime(t *testing.T) {
	usecase := &fakeFeeConfigUsecase{}
	request := validSetFeeConfig()
	request.EffectiveFrom = &timestamppb.Timestamp{Seconds: 253402300800} // year 10000

	_, err := NewAdminHandler(usecase, discardLogger()).SetFeeConfig(adminContext(uuid.NewString(), "admin"), request)

	if code := status.Code(err); code != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", code)
	}
	if usecase.calls != 0 {
		t.Error("usecase must not be called")
	}
}
