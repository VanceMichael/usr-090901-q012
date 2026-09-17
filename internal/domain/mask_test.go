package domain

import "testing"

func TestAmountBand(t *testing.T) {
	r := testRules(t)
	cases := map[int64]string{
		0:         "10万以下",
		9999999:   "10万以下",
		10000000:  "10万-50万",
		66000000:  "50万-100万",
		188000000: "100万-500万",
		520000000: "500万以上",
	}
	for cents, want := range cases {
		if got := r.AmountBandFor(cents); got != want {
			t.Fatalf("band(%d) = %s, want %s", cents, got, want)
		}
	}
}

func TestMaskContact(t *testing.T) {
	if got := MaskContact("full", "+86-138-0000-1234"); got != "+86-138-0000-1234" {
		t.Fatalf("full = %s", got)
	}
	if got := MaskContact("masked", "+86-138-0000-1234"); got != "+86-****34" {
		t.Fatalf("masked = %s", got)
	}
	if got := MaskContact("hidden", "+86-138-0000-1234"); got != "" {
		t.Fatalf("hidden = %q", got)
	}
	if got := MaskContact("masked", "12345"); got != "****" {
		t.Fatalf("short masked = %s", got)
	}
}

func TestMaskForUnknownRoleIsStrictest(t *testing.T) {
	r := testRules(t)
	m := r.MaskFor("nobody")
	if m.Amount != "hidden" || m.Contact != "hidden" {
		t.Fatalf("mask = %+v", m)
	}
	if got := r.MaskFor("teller"); got.Amount != "band" || got.Contact != "hidden" {
		t.Fatalf("teller mask = %+v", got)
	}
}
