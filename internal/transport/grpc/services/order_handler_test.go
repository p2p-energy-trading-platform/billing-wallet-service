package services

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"billing-wallet-service/internal/domain"

	"github.com/google/uuid"
	billingv1 "github.com/p2p-energy-trading-platform/go-sdk/gen/gridx/billing/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeReservationUsecase struct {
	reservation domain.Reservation
	err         error
	request     domain.ReserveRequest
	calls       int
	orderID     uuid.UUID
	reason      domain.ReleaseReason
	released    int64
}

func (f *fakeReservationUsecase) ReserveCredits(_ context.Context, request domain.ReserveRequest) (domain.Reservation, error) {
	f.calls++
	f.request = request
	return f.reservation, f.err
}

func (f *fakeReservationUsecase) ReleaseReservation(_ context.Context, orderID uuid.UUID, reason domain.ReleaseReason) (int64, error) {
	f.calls++
	f.orderID = orderID
	f.reason = reason
	return f.released, f.err
}

func TestReserveCreditsMapsTheRequestAndResponse(t *testing.T) {
	userID, orderID, holdID := uuid.New(), uuid.New(), uuid.New()
	expiresAt := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	usecase := &fakeReservationUsecase{reservation: domain.Reservation{
		Hold:           domain.Hold{ID: holdID, AmountTotal: 4646},
		AvailableAfter: 5354,
	}}

	response, err := NewOrderHandler(usecase, &fakeSettlementUsecase{}, discardLogger()).ReserveCredits(context.Background(), &billingv1.ReserveCreditsRequest{
		UserId:         userID.String(),
		OrderId:        orderID.String(),
		OrderType:      billingv1.OrderType_ORDER_TYPE_MARKET,
		QtyWh:          1000,
		MaxPricePerKwh: 4550,
		ExpiresAt:      timestamppb.New(expiresAt),
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := usecase.request
	if got.UserID != userID || got.OrderID != orderID || got.OrderType != domain.OrderTypeMarket || got.QtyWh != 1000 || got.MaxPricePerKwh != 4550 {
		t.Errorf("request = %+v", got)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(expiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, expiresAt)
	}
	if response.GetHoldId() != holdID.String() || response.GetReservedAmount() != 4646 || response.GetAvailableAfter() != 5354 {
		t.Errorf("response = %v", response)
	}
}

func TestReserveCreditsLeavesExpiryUnsetWhenNotGiven(t *testing.T) {
	usecase := &fakeReservationUsecase{}

	_, err := NewOrderHandler(usecase, &fakeSettlementUsecase{}, discardLogger()).ReserveCredits(context.Background(), &billingv1.ReserveCreditsRequest{
		UserId:  uuid.NewString(),
		OrderId: uuid.NewString(),
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usecase.request.ExpiresAt != nil {
		t.Errorf("ExpiresAt = %v, want nil", usecase.request.ExpiresAt)
	}
}

func TestReserveCreditsRejectsMalformedIDsBeforeTheUsecase(t *testing.T) {
	tests := []*billingv1.ReserveCreditsRequest{
		{UserId: "nope", OrderId: uuid.NewString()},
		{UserId: uuid.NewString(), OrderId: "nope"},
	}

	for _, request := range tests {
		usecase := &fakeReservationUsecase{}

		_, err := NewOrderHandler(usecase, &fakeSettlementUsecase{}, discardLogger()).ReserveCredits(context.Background(), request)

		if code := status.Code(err); code != codes.InvalidArgument {
			t.Errorf("code = %v, want InvalidArgument", code)
		}
		if usecase.calls != 0 {
			t.Error("usecase must not be called")
		}
	}
}

func TestOrderTypesAndReasonsMap(t *testing.T) {
	orderTypes := map[billingv1.OrderType]domain.OrderType{
		billingv1.OrderType_ORDER_TYPE_LIMIT:       domain.OrderTypeLimit,
		billingv1.OrderType_ORDER_TYPE_MARKET:      domain.OrderTypeMarket,
		billingv1.OrderType_ORDER_TYPE_UNSPECIFIED: "",
	}
	for from, want := range orderTypes {
		if got := orderTypeFromProto(from); got != want {
			t.Errorf("orderTypeFromProto(%v) = %q, want %q", from, got, want)
		}
	}

	reasons := map[billingv1.ReleaseReason]domain.ReleaseReason{
		billingv1.ReleaseReason_RELEASE_REASON_CANCELLED:   domain.ReleaseCancelled,
		billingv1.ReleaseReason_RELEASE_REASON_EXPIRED:     domain.ReleaseExpired,
		billingv1.ReleaseReason_RELEASE_REASON_FILLED:      domain.ReleaseFilled,
		billingv1.ReleaseReason_RELEASE_REASON_REJECTED:    domain.ReleaseRejected,
		billingv1.ReleaseReason_RELEASE_REASON_UNSPECIFIED: "",
	}
	for from, want := range reasons {
		if got := releaseReasonFromProto(from); got != want {
			t.Errorf("releaseReasonFromProto(%v) = %q, want %q", from, got, want)
		}
	}
}

func TestReleaseReservationMapsTheRequestAndResponse(t *testing.T) {
	orderID := uuid.New()
	usecase := &fakeReservationUsecase{released: 600}

	response, err := NewOrderHandler(usecase, &fakeSettlementUsecase{}, discardLogger()).ReleaseReservation(context.Background(), &billingv1.ReleaseReservationRequest{
		OrderId: orderID.String(),
		Reason:  billingv1.ReleaseReason_RELEASE_REASON_FILLED,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usecase.orderID != orderID || usecase.reason != domain.ReleaseFilled {
		t.Errorf("called with %v, %q", usecase.orderID, usecase.reason)
	}
	if response.GetReleasedAmount() != 600 {
		t.Errorf("released_amount = %d, want 600", response.GetReleasedAmount())
	}
}

func TestOrderHandlerMapsUsecaseErrors(t *testing.T) {
	usecase := &fakeReservationUsecase{err: errors.Join(domain.ErrInsufficientFunds)}
	handler := NewOrderHandler(usecase, &fakeSettlementUsecase{}, discardLogger())

	_, reserveErr := handler.ReserveCredits(context.Background(), &billingv1.ReserveCreditsRequest{
		UserId:  uuid.NewString(),
		OrderId: uuid.NewString(),
	})
	_, releaseErr := handler.ReleaseReservation(context.Background(), &billingv1.ReleaseReservationRequest{
		OrderId: uuid.NewString(),
	})

	for _, err := range []error{reserveErr, releaseErr} {
		if code := status.Code(err); code != codes.FailedPrecondition {
			t.Errorf("code = %v, want FailedPrecondition", code)
		}
	}
}

type fakeSettlementUsecase struct {
	result  domain.SettlementResult
	err     error
	request domain.SettleRequest
	calls   int
}

func (f *fakeSettlementUsecase) SettleTrade(_ context.Context, request domain.SettleRequest) (domain.SettlementResult, error) {
	f.calls++
	f.request = request
	return f.result, f.err
}

func settleTradeRequest() *billingv1.SettleTradeRequest {
	return &billingv1.SettleTradeRequest{
		TradeId: uuid.NewString(), BuyOrderId: uuid.NewString(), SellOrderId: uuid.NewString(),
		BuyerUserId: uuid.NewString(), SellerUserId: uuid.NewString(),
		QtyWh: 1000, PricePerKwh: 4550, BuyOrderFinal: true,
		ExecutedAt: timestamppb.New(time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)),
	}
}

func TestSettleTradeMapsTheRequestAndResponse(t *testing.T) {
	settlementID := uuid.New()
	settlements := &fakeSettlementUsecase{result: domain.SettlementResult{
		SettlementID: settlementID, TradeValue: 4550, BuyerFee: 46, SellerFee: 46, BuyerReleasedRemainder: 50, AlreadySettled: true,
	}}
	request := settleTradeRequest()

	response, err := NewOrderHandler(&fakeReservationUsecase{}, settlements, discardLogger()).SettleTrade(context.Background(), request)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := settlements.request
	if got.TradeID.String() != request.GetTradeId() || got.BuyOrderID.String() != request.GetBuyOrderId() ||
		got.SellOrderID.String() != request.GetSellOrderId() || got.BuyerUserID.String() != request.GetBuyerUserId() ||
		got.SellerUserID.String() != request.GetSellerUserId() || got.QtyWh != 1000 || got.PricePerKwh != 4550 ||
		!got.BuyOrderFinal || !got.ExecutedAt.Equal(request.GetExecutedAt().AsTime()) {
		t.Errorf("request = %+v", got)
	}
	if response.GetSettlementId() != settlementID.String() || response.GetTradeValue() != 4550 || response.GetBuyerFee() != 46 ||
		response.GetSellerFee() != 46 || response.GetBuyerReleasedRemainder() != 50 || !response.GetAlreadySettled() {
		t.Errorf("response = %v", response)
	}
}

func TestSettleTradeRejectsMalformedInputBeforeTheUsecase(t *testing.T) {
	tests := map[string]func(*billingv1.SettleTradeRequest){
		"bad trade id":      func(r *billingv1.SettleTradeRequest) { r.TradeId = "nope" },
		"bad buy order":     func(r *billingv1.SettleTradeRequest) { r.BuyOrderId = "nope" },
		"bad sell order":    func(r *billingv1.SettleTradeRequest) { r.SellOrderId = "nope" },
		"bad buyer":         func(r *billingv1.SettleTradeRequest) { r.BuyerUserId = "nope" },
		"bad seller":        func(r *billingv1.SettleTradeRequest) { r.SellerUserId = "nope" },
		"no execution time": func(r *billingv1.SettleTradeRequest) { r.ExecutedAt = nil },
		"execution time out of range": func(r *billingv1.SettleTradeRequest) {
			r.ExecutedAt = &timestamppb.Timestamp{Seconds: 253402300800}
		},
	}

	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			settlements := &fakeSettlementUsecase{}
			request := settleTradeRequest()
			change(request)

			_, err := NewOrderHandler(&fakeReservationUsecase{}, settlements, discardLogger()).SettleTrade(context.Background(), request)

			if code := status.Code(err); code != codes.InvalidArgument {
				t.Errorf("code = %v, want InvalidArgument", code)
			}
			if settlements.calls != 0 {
				t.Error("usecase must not be called")
			}
		})
	}
}

func TestSettleTradeMapsUsecaseErrors(t *testing.T) {
	settlements := &fakeSettlementUsecase{err: domain.ErrPriceExceedsReservation}

	_, err := NewOrderHandler(&fakeReservationUsecase{}, settlements, discardLogger()).SettleTrade(context.Background(), settleTradeRequest())

	if code := status.Code(err); code != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", code)
	}
}

func TestSettleTradeNamesTheFirstBadFieldEveryTime(t *testing.T) {
	request := settleTradeRequest()
	request.TradeId, request.SellerUserId = "nope", "nope"

	for range 20 {
		_, err := NewOrderHandler(&fakeReservationUsecase{}, &fakeSettlementUsecase{}, discardLogger()).SettleTrade(context.Background(), request)

		if message := status.Convert(err).Message(); !strings.Contains(message, `"trade_id"`) {
			t.Fatalf("error = %q, want it to name trade_id", message)
		}
	}
}
