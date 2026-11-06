package hawkingaudit

import (
	"math/big"
	"testing"

	"github.com/PlatONnetwork/PlatON-Go/common"
)

func TestRestrictingOverpayDoesNotCountBackedPrincipal(t *testing.T) {
	resetForTest()
	account := common.HexToAddress("0x10000000000000000000000000000000000000aa")

	// NeedRelease 50, deposit 80: 50 is paid now, 30 is added to Cache, the
	// full 80 is still scheduled. The cash just paid is not itself an overpay.
	over := RealizeDebtPayment(account, big.NewInt(50))
	if over.Sign() != 0 {
		t.Fatalf("first debt payment = %s, want 0", over)
	}
	NoteDeposit(account, big.NewInt(50), big.NewInt(30))

	// Epoch release pays the 30 that was actually added to Cache.
	over = RealizeCacheRelease(account, big.NewInt(30))
	if over.Sign() != 0 {
		t.Fatalf("cache release = %s, want 0", over)
	}

	// The scheduled 50 is paid later as NeedRelease. That is the only extra cash.
	over = RealizeDebtPayment(account, big.NewInt(50))
	if over.Cmp(big.NewInt(50)) != 0 {
		t.Fatalf("second debt payment = %s, want 50", over)
	}
	over = RealizeDebtPayment(account, big.NewInt(1))
	if over.Sign() != 0 {
		t.Fatalf("tracker not exhausted, got %s", over)
	}
}

func TestStuckIncreaseReportsDeltaOnce(t *testing.T) {
	resetForTest()
	account := common.HexToAddress("0x10000000000000000000000000000000000000bb")
	first := StuckIncrease(account, big.NewInt(40))
	if first.Cmp(big.NewInt(40)) != 0 {
		t.Fatalf("first stuck = %s, want 40", first)
	}
	again := StuckIncrease(account, big.NewInt(40))
	if again.Sign() != 0 {
		t.Fatalf("repeat stuck = %s, want 0", again)
	}
	more := StuckIncrease(account, big.NewInt(55))
	if more.Cmp(big.NewInt(15)) != 0 {
		t.Fatalf("increase = %s, want 15", more)
	}
	cleared := StuckIncrease(account, big.NewInt(0))
	if cleared.Sign() != 0 {
		t.Fatalf("clear = %s, want 0", cleared)
	}
}
