package domain

import (
	"errors"
	"testing"
)

func TestHoldStatusTransitions(t *testing.T) {
	legal := map[HoldStatus][]HoldStatus{
		HoldActive: {HoldConsumed, HoldReleased, HoldExpired},
	}
	all := []HoldStatus{HoldActive, HoldConsumed, HoldReleased, HoldExpired}

	assertTransitions(t, all, legal, HoldStatus.CanTransitionTo)
}

func TestDepositStatusTransitions(t *testing.T) {
	legal := map[DepositStatus][]DepositStatus{
		DepositPending: {DepositCompleted, DepositFailed, DepositExpired},
		// Late success after expiry: the money was received, so it is still credited.
		DepositExpired:   {DepositCompleted},
		DepositCompleted: {DepositRefunded, DepositDisputed},
	}
	all := []DepositStatus{
		DepositPending, DepositCompleted, DepositFailed,
		DepositExpired, DepositRefunded, DepositDisputed,
	}

	assertTransitions(t, all, legal, DepositStatus.CanTransitionTo)
}

func TestWithdrawalStatusTransitions(t *testing.T) {
	legal := map[WithdrawalStatus][]WithdrawalStatus{
		WithdrawalPending:  {WithdrawalApproved, WithdrawalRejected, WithdrawalCancelled, WithdrawalFailed},
		WithdrawalApproved: {WithdrawalPaid, WithdrawalFailed},
	}
	all := []WithdrawalStatus{
		WithdrawalPending, WithdrawalApproved, WithdrawalPaid,
		WithdrawalRejected, WithdrawalCancelled, WithdrawalFailed,
	}

	assertTransitions(t, all, legal, WithdrawalStatus.CanTransitionTo)
}

// assertTransitions checks every from/to pair, so anything not listed as legal
// must be refused, including a status moving to itself.
func assertTransitions[S comparable](
	t *testing.T,
	all []S,
	legal map[S][]S,
	canTransition func(from, to S) bool,
) {
	t.Helper()

	for _, from := range all {
		for _, to := range all {
			want := false
			for _, allowed := range legal[from] {
				if allowed == to {
					want = true
				}
			}
			if got := canTransition(from, to); got != want {
				t.Errorf("transition %v -> %v = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestWalletTotal(t *testing.T) {
	wallet := Wallet{Available: 700, Reserved: 300}

	if got := wallet.Total(); got != 1000 {
		t.Errorf("Total() = %d, want 1000", got)
	}
}

func TestHoldRemaining(t *testing.T) {
	hold := Hold{AmountTotal: 1000, AmountConsumed: 250, AmountReleased: 100}

	if got := hold.Remaining(); got != 650 {
		t.Errorf("Remaining() = %d, want 650", got)
	}
}

func TestValidateBalanced(t *testing.T) {
	tests := []struct {
		name    string
		lines   []LedgerLine
		wantErr bool
	}{
		{
			name:  "settlement lines sum to zero",
			lines: []LedgerLine{{Amount: -1010}, {Amount: 990}, {Amount: 20}},
		},
		{
			name:    "lines that do not sum to zero",
			lines:   []LedgerLine{{Amount: -1010}, {Amount: 990}},
			wantErr: true,
		},
		{
			name:    "no lines",
			lines:   nil,
			wantErr: true,
		},
		{
			name:    "a zero amount line",
			lines:   []LedgerLine{{Amount: 0}, {Amount: 100}, {Amount: -100}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateBalanced(tt.lines)

			if tt.wantErr && !errors.Is(err, ErrUnbalancedJournal) {
				t.Fatalf("ValidateBalanced() error = %v, want ErrUnbalancedJournal", err)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidateBalanced() unexpected error: %v", err)
			}
		})
	}
}

func TestWalletEnsureActive(t *testing.T) {
	tests := []struct {
		status  WalletStatus
		wantErr error
	}{
		{status: WalletActive},
		{status: WalletFrozen, wantErr: ErrWalletFrozen},
		{status: WalletClosed, wantErr: ErrWalletFrozen},
	}

	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			err := Wallet{Status: tt.status}.EnsureActive()

			if !errors.Is(err, tt.wantErr) {
				t.Errorf("EnsureActive() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
