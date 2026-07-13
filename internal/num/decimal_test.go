package num

import "testing"

func TestQuantize(t *testing.T) {
	v := Must("1.23456")
	step := Must("0.01")
	if got := v.QuantizeDown(step).String(); got != "1.23" {
		t.Fatalf("down=%s", got)
	}
	if got := v.QuantizeUp(step).String(); got != "1.24" {
		t.Fatalf("up=%s", got)
	}
}

func TestExactArithmetic(t *testing.T) {
	got := Must("0.1").Add(Must("0.2"))
	if got.Cmp(Must("0.3")) != 0 {
		t.Fatalf("got %s", got.String())
	}
}
