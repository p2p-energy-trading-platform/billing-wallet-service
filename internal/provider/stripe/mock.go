package stripe

import (
	"context"
	"net/http"
	"sync"
)

type MockProvider struct {
	mu       sync.Mutex
	Sessions map[string]CheckoutStatus
	ParseFn  func(headers http.Header, body []byte) (WebhookEvent, error)
}

func NewMockProvider() *MockProvider {
	return &MockProvider{
		Sessions: make(map[string]CheckoutStatus),
	}
}

func (m *MockProvider) Name() string {
	return "mock"
}

func (m *MockProvider) CreateCheckout(
	_ context.Context,
	in CheckoutInput,
) (CheckoutResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ref := "cs_test_" + in.DepositID

	m.Sessions[ref] = CheckoutStatus{
		Status:        "open",
		PaymentStatus: "unpaid",
		Currency:      in.ChargeCurrency,
		AmountTotal:   in.ChargeAmount,
	}

	return CheckoutResult{
		ProviderRef: ref,
		CheckoutURL: "https://checkout.stripe.mock/pay/" + ref,
	}, nil
}

func (m *MockProvider) ParseWebhook(
	headers http.Header,
	body []byte,
) (WebhookEvent, error) {
	if m.ParseFn != nil {
		return m.ParseFn(headers, body)
	}

	client := NewClient("sk_test_mock", "whsec_test", 0)

	return client.ParseWebhook(headers, body)
}

func (m *MockProvider) GetCheckoutStatus(
	_ context.Context,
	ref string,
) (CheckoutStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	status, ok := m.Sessions[ref]
	if !ok {
		return CheckoutStatus{
			Status:        "expired",
			PaymentStatus: "unpaid",
		}, nil
	}

	return status, nil
}

var _ PaymentProvider = (*MockProvider)(nil)
