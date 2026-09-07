package main

import (
	"testing"
)

func TestApplyLotCap(t *testing.T) {
	if got, capped := applyLotCap(645, 500); got != 500 || !capped {
		t.Fatalf("applyLotCap(645, 500) = (%d, %v), want (500, true)", got, capped)
	}
	if got, capped := applyLotCap(31, 500); got != 31 || capped {
		t.Fatalf("applyLotCap(31, 500) = (%d, %v), want (31, false)", got, capped)
	}
	if got, capped := applyLotCap(500, 500); got != 500 || capped {
		t.Fatalf("applyLotCap(500, 500) = (%d, %v), want (500, false)", got, capped)
	}
	if got, capped := applyLotCap(10, 0); got != 10 || capped {
		t.Fatalf("applyLotCap(10, 0) = (%d, %v), want (10, false)", got, capped)
	}
}
