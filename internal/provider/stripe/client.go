package stripe

import (
	"billing-wallet-service/internal/domain"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	stripego "github.com/stripe/stripe-go/v87"
	stripewebhook "github.com/stripe/stripe-go/v87/webhook"
)

type CheckoutInput struct {
	DepositID      string
	WalletID       string
	UserID         string
	CreditsAmount  int64
	ChargeCurrency string
	ChargeAmount   int64
	SuccessURL     string
	CancelURL      string
	ExpiresAt      time.Time
	IdempotencyKey string
}

type CheckoutResult struct {
	ProviderRef string
	CheckoutURL string
}

type WebhookEvent struct {
	EventID       string
	Type          string
	ProviderRef   string
	PaymentIntent string
	PaymentStatus string
	Currency      string
	AmountTotal   int64
}

type CheckoutStatus struct {
	Status        string
	PaymentStatus string
	PaymentIntent string
	Currency      string
	AmountTotal   int64
}

type PaymentProvider interface {
	Name() string
	CreateCheckout(ctx context.Context, in CheckoutInput) (CheckoutResult, error)
	ParseWebhook(headers http.Header, body []byte) (WebhookEvent, error)
	GetCheckoutStatus(ctx context.Context, ref string) (CheckoutStatus, error)
}

type Client struct {
	stripeClient     *stripego.Client
	webhookSecret    string
	webhookTolerance time.Duration
}

func NewClient(secretKey, webhookSecret string, tolerance time.Duration) *Client {
	if tolerance <= 0 {
		tolerance = 5 * time.Minute
	}

	return &Client{
		stripeClient:     stripego.NewClient(secretKey),
		webhookSecret:    webhookSecret,
		webhookTolerance: tolerance,
	}
}

func (c *Client) Name() string { return "stripe" }

func (c *Client) CreateCheckout(ctx context.Context, in CheckoutInput) (CheckoutResult, error) {
	params := &stripego.CheckoutSessionCreateParams{
		Mode:              stripego.String(string(stripego.CheckoutSessionModePayment)),
		SuccessURL:        stripego.String(in.SuccessURL),
		CancelURL:         stripego.String(in.CancelURL),
		ClientReferenceID: stripego.String(in.DepositID),

		Metadata: map[string]string{
			"deposit_id":      in.DepositID,
			"wallet_id":       in.WalletID,
			"user_id":         in.UserID,
			"credits_amount":  fmt.Sprintf("%d", in.CreditsAmount),
			"charge_currency": strings.ToLower(in.ChargeCurrency),
		},

		LineItems: []*stripego.CheckoutSessionCreateLineItemParams{
			{
				Quantity: stripego.Int64(1),
				PriceData: &stripego.CheckoutSessionCreateLineItemPriceDataParams{
					Currency:   stripego.String(strings.ToLower(in.ChargeCurrency)),
					UnitAmount: stripego.Int64(in.ChargeAmount),
					ProductData: &stripego.CheckoutSessionCreateLineItemPriceDataProductDataParams{
						Name: stripego.String("Wallet top-up"),
					},
				},
			},
		},
	}

	if !in.ExpiresAt.IsZero() {
		params.ExpiresAt = stripego.Int64(in.ExpiresAt.Unix())
	}

	if in.IdempotencyKey != "" {
		params.IdempotencyKey = stripego.String(in.IdempotencyKey)
	}

	session, err := c.stripeClient.V1CheckoutSessions.Create(ctx, params)
	if err != nil {
		return CheckoutResult{}, fmt.Errorf(
			"%w: stripe create checkout: %v",
			domain.ErrUnavailable,
			err,
		)
	}

	return CheckoutResult{
		ProviderRef: session.ID,
		CheckoutURL: session.URL,
	}, nil
}

func (c *Client) ParseWebhook(
	headers http.Header,
	body []byte,
) (WebhookEvent, error) {
	signature := headers.Get("Stripe-Signature")

	if signature == "" {
		return WebhookEvent{}, fmt.Errorf(
			"%w: missing Stripe-Signature header",
			domain.ErrWebhookSignatureInvalid,
		)
	}

	event, err := stripewebhook.ConstructEventWithOptions(
		body,
		signature,
		c.webhookSecret,
		stripewebhook.ConstructEventOptions{
			Tolerance: c.webhookTolerance,
		},
	)

	if err != nil {
		if isWebhookSignatureError(err) {
			return WebhookEvent{}, fmt.Errorf(
				"%w: %v",
				domain.ErrWebhookSignatureInvalid,
				err,
			)
		}

		return WebhookEvent{}, fmt.Errorf(
			"construct Stripe webhook event: %w",
			err,
		)
	}

	if event.Data == nil {
		return WebhookEvent{}, errors.New("Stripe webhook has no event data")
	}

	var checkoutSession struct {
		ID            string          `json:"id"`
		PaymentStatus string          `json:"payment_status"`
		Currency      string          `json:"currency"`
		AmountTotal   int64           `json:"amount_total"`
		PaymentIntent json.RawMessage `json:"payment_intent"`
	}

	if err := json.Unmarshal(event.Data.Raw, &checkoutSession); err != nil {
		return WebhookEvent{}, fmt.Errorf(
			"decode Stripe checkout session: %w",
			err,
		)
	}

	paymentIntentID, err := expandableID(checkoutSession.PaymentIntent)
	if err != nil {
		return WebhookEvent{}, fmt.Errorf(
			"decode Stripe payment intent: %w",
			err,
		)
	}

	return WebhookEvent{
		EventID:       event.ID,
		Type:          string(event.Type),
		ProviderRef:   checkoutSession.ID,
		PaymentIntent: paymentIntentID,
		PaymentStatus: checkoutSession.PaymentStatus,
		Currency:      strings.ToLower(checkoutSession.Currency),
		AmountTotal:   checkoutSession.AmountTotal,
	}, nil
}

func (c *Client) GetCheckoutStatus(
	ctx context.Context,
	ref string,
) (CheckoutStatus, error) {
	session, err := c.stripeClient.V1CheckoutSessions.Retrieve(
		ctx,
		ref,
		&stripego.CheckoutSessionRetrieveParams{},
	)

	if err != nil {
		return CheckoutStatus{}, fmt.Errorf(
			"%w: stripe retrieve checkout session: %v",
			domain.ErrUnavailable,
			err,
		)
	}

	var paymentIntentID string

	if session.PaymentIntent != nil {
		paymentIntentID = session.PaymentIntent.ID
	}

	return CheckoutStatus{
		Status:        string(session.Status),
		PaymentStatus: string(session.PaymentStatus),
		PaymentIntent: paymentIntentID,
		Currency:      strings.ToLower(string(session.Currency)),
		AmountTotal:   session.AmountTotal,
	}, nil
}

// expandableID handles Stripe expandable fields.
// Stripe can return a PaymentIntent as either an ID string
// or an expanded object containing its ID.
func expandableID(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)

	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", nil
	}

	var id string

	if err := json.Unmarshal(raw, &id); err == nil {
		return id, nil
	}

	var expanded struct {
		ID string `json:"id"`
	}

	if err := json.Unmarshal(raw, &expanded); err != nil {
		return "", err
	}

	return expanded.ID, nil
}

func isWebhookSignatureError(err error) bool {
	return errors.Is(err, stripewebhook.ErrInvalidHeader) ||
		errors.Is(err, stripewebhook.ErrNoValidSignature) ||
		errors.Is(err, stripewebhook.ErrNotSigned) ||
		errors.Is(err, stripewebhook.ErrTooOld) ||
		errors.Is(err, stripewebhook.ErrEmptySecret)
}

var _ PaymentProvider = (*Client)(nil)
