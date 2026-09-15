package node

import "testing"

func TestStateNames(t *testing.T) {
	for s, want := range invoiceStateNames {
		if got := s.String(); got != want {
			t.Errorf("invoice state %d prints %q, want %q", uint8(s), got, want)
		}
	}
	if got := InvoiceState(200).String(); got != "invoice state(200)" {
		t.Errorf("unknown invoice state prints %q", got)
	}

	for s, want := range paymentStateNames {
		if got := s.String(); got != want {
			t.Errorf("payment state %d prints %q, want %q", uint8(s), got, want)
		}
	}
	if got := PaymentState(200).String(); got != "payment state(200)" {
		t.Errorf("unknown payment state prints %q", got)
	}
}

// TestOnlyAcceptedIsClaimable: the bridge pays out against a locked-in HTLC
// and nothing else, so every other state must report itself unclaimable.
func TestOnlyAcceptedIsClaimable(t *testing.T) {
	for s := range invoiceStateNames {
		if got := s.Claimable(); got != (s == InvoiceAccepted) {
			t.Errorf("%v.Claimable() = %v", s, got)
		}
	}
	if InvoiceState(200).Claimable() {
		t.Error("an unknown invoice state must not be claimable")
	}
}
