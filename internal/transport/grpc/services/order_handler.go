package services

import (
	"context"
	"log/slog"

	"billing-wallet-service/internal/domain"

	"github.com/google/uuid"
	billingv1 "github.com/p2p-energy-trading-platform/go-sdk/gen/gridx/billing/v1"
)

type ReservationUsecase interface {
	ReserveCredits(ctx context.Context, request domain.ReserveRequest) (domain.Reservation, error)
	ReleaseReservation(ctx context.Context, orderID uuid.UUID, reason domain.ReleaseReason) (int64, error)
}

type OrderHandler struct {
	billingv1.UnimplementedOrderBillingServiceServer
	usecase ReservationUsecase
	logger  *slog.Logger
}

func NewOrderHandler(usecase ReservationUsecase, logger *slog.Logger) *OrderHandler {
	return &OrderHandler{usecase: usecase, logger: logger}
}

func (h *OrderHandler) ReserveCredits(ctx context.Context, request *billingv1.ReserveCreditsRequest) (*billingv1.ReserveCreditsResponse, error) {
	userID, err := parseUUID("user_id", request.GetUserId())
	if err != nil {
		return nil, toStatus(h.logger, err)
	}

	orderID, err := parseUUID("order_id", request.GetOrderId())
	if err != nil {
		return nil, toStatus(h.logger, err)
	}

	reserve := domain.ReserveRequest{
		UserID:         userID,
		OrderID:        orderID,
		OrderType:      orderTypeFromProto(request.GetOrderType()),
		QtyWh:          request.GetQtyWh(),
		MaxPricePerKwh: request.GetMaxPricePerKwh(),
	}
	if request.GetExpiresAt() != nil {
		expiresAt := request.GetExpiresAt().AsTime()
		reserve.ExpiresAt = &expiresAt
	}

	reservation, err := h.usecase.ReserveCredits(ctx, reserve)
	if err != nil {
		return nil, toStatus(h.logger, err)
	}

	return &billingv1.ReserveCreditsResponse{
		HoldId:         reservation.Hold.ID.String(),
		ReservedAmount: reservation.Hold.AmountTotal,
		AvailableAfter: reservation.AvailableAfter,
	}, nil
}

func (h *OrderHandler) ReleaseReservation(ctx context.Context, request *billingv1.ReleaseReservationRequest) (*billingv1.ReleaseReservationResponse, error) {
	orderID, err := parseUUID("order_id", request.GetOrderId())
	if err != nil {
		return nil, toStatus(h.logger, err)
	}

	released, err := h.usecase.ReleaseReservation(ctx, orderID, releaseReasonFromProto(request.GetReason()))
	if err != nil {
		return nil, toStatus(h.logger, err)
	}

	return &billingv1.ReleaseReservationResponse{ReleasedAmount: released}, nil
}

func parseUUID(field, value string) (uuid.UUID, error) {
	parsed, err := uuid.Parse(value)
	if err != nil {
		return uuid.Nil, domain.NewValidationError(field, "must be a UUID")
	}

	return parsed, nil
}

// orderTypeFromProto returns "" for an unset type, which validation then rejects.
func orderTypeFromProto(orderType billingv1.OrderType) domain.OrderType {
	switch orderType {
	case billingv1.OrderType_ORDER_TYPE_LIMIT:
		return domain.OrderTypeLimit
	case billingv1.OrderType_ORDER_TYPE_MARKET:
		return domain.OrderTypeMarket
	default:
		return ""
	}
}

// releaseReasonFromProto returns "" for an unset reason, which validation then rejects.
func releaseReasonFromProto(reason billingv1.ReleaseReason) domain.ReleaseReason {
	switch reason {
	case billingv1.ReleaseReason_RELEASE_REASON_CANCELLED:
		return domain.ReleaseCancelled
	case billingv1.ReleaseReason_RELEASE_REASON_EXPIRED:
		return domain.ReleaseExpired
	case billingv1.ReleaseReason_RELEASE_REASON_FILLED:
		return domain.ReleaseFilled
	case billingv1.ReleaseReason_RELEASE_REASON_REJECTED:
		return domain.ReleaseRejected
	default:
		return ""
	}
}
