package services

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"billing-wallet-service/internal/domain"

	billingv1 "github.com/p2p-energy-trading-platform/go-sdk/gen/gridx/billing/v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	// userRoleMetadataKey carries the role the API Gateway verified from the token.
	userRoleMetadataKey = "x-user-role"
	adminRole           = "admin"
)

type FeeConfigUsecase interface {
	SetFeeConfig(ctx context.Context, request domain.SetFeeConfigRequest) (domain.FeeConfig, error)
}

type AdminHandler struct {
	billingv1.UnimplementedAdminBillingServiceServer
	feeConfigs FeeConfigUsecase
	logger     *slog.Logger
}

func NewAdminHandler(feeConfigs FeeConfigUsecase, logger *slog.Logger) *AdminHandler {
	return &AdminHandler{feeConfigs: feeConfigs, logger: logger}
}

func (h *AdminHandler) SetFeeConfig(ctx context.Context, request *billingv1.SetFeeConfigRequest) (*billingv1.SetFeeConfigResponse, error) {
	if !callerIsAdmin(ctx) {
		return nil, toStatus(h.logger, domain.ErrCallerNotAllowed)
	}

	actorID, err := callerUserID(ctx)
	if err != nil {
		return nil, err
	}

	// A missing rate must not quietly become a free side of every trade.
	if request.BuyerFeeBps == nil {
		return nil, toStatus(h.logger, domain.NewValidationError("buyer_fee_bps", "is required"))
	}
	if request.SellerFeeBps == nil {
		return nil, toStatus(h.logger, domain.NewValidationError("seller_fee_bps", "is required"))
	}

	setFeeConfig := domain.SetFeeConfigRequest{
		IdempotencyKey: request.GetIdempotencyKey(),
		BuyerFeeBps:    request.GetBuyerFeeBps(),
		SellerFeeBps:   request.GetSellerFeeBps(),
		ActorID:        actorID,
	}
	if request.GetEffectiveFrom() != nil {
		if err := request.GetEffectiveFrom().CheckValid(); err != nil {
			return nil, toStatus(h.logger, domain.NewValidationError("effective_from", "is out of range"))
		}

		// Postgres keeps microseconds, so a retry compares equal to the stored value.
		effectiveFrom := request.GetEffectiveFrom().AsTime().Truncate(time.Microsecond)
		setFeeConfig.EffectiveFrom = &effectiveFrom
	}

	config, err := h.feeConfigs.SetFeeConfig(ctx, setFeeConfig)
	if err != nil {
		return nil, toStatus(h.logger, err)
	}

	h.logger.Info("fee config set",
		"fee_config_id", config.ID,
		"reviewed_by", actorID,
		"buyer_fee_bps", config.BuyerFeeBps,
		"seller_fee_bps", config.SellerFeeBps,
		"effective_from", config.EffectiveFrom,
	)

	return &billingv1.SetFeeConfigResponse{FeeConfig: feeConfigToProto(config)}, nil
}

func callerIsAdmin(ctx context.Context) bool {
	md, _ := metadata.FromIncomingContext(ctx)
	roles := md.Get(userRoleMetadataKey)

	return len(roles) == 1 && strings.EqualFold(roles[0], adminRole)
}

func feeConfigToProto(config domain.FeeConfig) *billingv1.FeeConfig {
	converted := &billingv1.FeeConfig{
		FeeConfigId:   config.ID.String(),
		BuyerFeeBps:   config.BuyerFeeBps,
		SellerFeeBps:  config.SellerFeeBps,
		EffectiveFrom: timestamppb.New(config.EffectiveFrom),
		CreatedAt:     timestamppb.New(config.CreatedAt),
	}
	if config.CreatedBy != nil {
		converted.CreatedBy = config.CreatedBy.String()
	}

	return converted
}
