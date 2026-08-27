package main

import "testing"

func TestInAiQuietHours(t *testing.T) {
	cases := []struct {
		hour int
		want bool
	}{
		{22, true},
		{23, true},
		{0, true},
		{5, true},
		{8, true},
		{9, false},
		{10, false},
		{12, false},
		{17, false},
		{20, false},
		{21, false},
	}
	for _, c := range cases {
		if got := inAiQuietHours(c.hour); got != c.want {
			t.Errorf("inAiQuietHours(%d) = %v, want %v", c.hour, got, c.want)
		}
	}
}

func TestInAiQuietHoursBoundaries(t *testing.T) {
	if !inAiQuietHours(22) {
		t.Error("22:00 должен быть тихим часом")
	}
	if !inAiQuietHours(8) {
		t.Error("08:59 должен быть тихим часом")
	}
	if inAiQuietHours(9) {
		t.Error("09:00 должен быть рабочим часом")
	}
	if inAiQuietHours(21) {
		t.Error("21:59 должен быть рабочим часом")
	}
}