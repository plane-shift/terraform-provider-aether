package versions

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
)

func TestSemanticallyEqual(t *testing.T) {
	cases := []struct {
		conf, actual string
		want         bool
	}{
		{"1.37", "1.37.0", true}, {"1.37", "1.37.4", true}, {"1.37.0", "1.37.0", true},
		{"1.37.0", "1.37.1", false}, {"1.37", "1.36.9", false}, {"v1.37", "1.37.0", true},
		{"1.37", "", false}, {"", "", true}, {"bogus", "1.37.0", false},
		// Digit-bound, verified against real strconv.Atoi behaviour (NOT the
		// brief's literal pairing, which assumed overflow silently yields 0 -
		// it doesn't: Atoi/ParseInt CLAMPS an out-of-range value to
		// math.MaxInt64 while still returning strconv.ErrRange). parse()
		// discards that error (`p.major, _ = strconv.Atoi(m[1])`), so TWO
		// different, equally-unparseable major components both collapse to
		// the same MaxInt64 sentinel and compare as equal today - confirmed
		// with a throwaway repro: SemanticallyEqual of these two returns
		// true. Once the discarded error is propagated, both sides fail to
		// parse and the result must be false.
		{"99999999999999999999.37", "88888888888888888888.37", false},
	}
	for _, c := range cases {
		if got := SemanticallyEqual(c.conf, c.actual); got != c.want {
			t.Errorf("SemanticallyEqual(%q,%q) = %v, want %v", c.conf, c.actual, got, c.want)
		}
	}
}

func TestIsForward(t *testing.T) {
	for _, c := range []struct {
		from, to string
		want     bool
		wantErr  bool
	}{
		{"1.36", "1.37", true, false}, {"1.36.4", "1.37", true, false}, {"1.37", "1.36", false, false},
		{"1.37.0", "1.37.1", true, false}, {"1.37", "1.37", false, false}, {"x", "1.37", false, true},
		// Same digit-bound collision as TestSemanticallyEqual: today both
		// sides parse "successfully" to the same clamped sentinel major, so
		// IsForward returns (false, nil) with no error at all. It must
		// surface the discarded parse error instead.
		{"99999999999999999999.37", "88888888888888888888.37", false, true},
	} {
		got, err := IsForward(c.from, c.to)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("IsForward(%q,%q) = %v,%v", c.from, c.to, got, err)
		}
	}
}

// TestValue_SemanticEqualsHook was direction-reversed in the brief: the
// framework calls it as proposedNewValuable.StringSemanticEquals(ctx,
// priorValuable) - the RECEIVER is the NEW value (what the API/plan just
// produced), the ARGUMENT is the PRIOR value (config/state). See
// terraform-plugin-framework@v1.19.0
// internal/fwschemadata/value_semantic_equality_string.go:37. `true` means
// "keep the prior value in state" - i.e. the new value doesn't count as
// drift from what was configured.
//
// The brief's version called it as receiver=prior("1.37"), arg=new("1.37.0"),
// which happens to also return true here (SemanticallyEqual is symmetric on
// this pair) but exercises the interface backwards and would not catch a
// receiver/argument swap regression. Corrected per the review.
func TestValue_SemanticEqualsHook(t *testing.T) {
	// New value "1.37.4" (an explicit patch the API resolved) satisfies a
	// prior bare-minor "1.37": keep the prior value, no drift.
	newVal := NewK8sVersionValue("1.37.4")
	eq, diags := newVal.StringSemanticEquals(context.Background(), NewK8sVersionValue("1.37"))
	if diags.HasError() || !eq {
		t.Fatalf("eq=%v diags=%v", eq, diags)
	}

	// A bare new value "1.37" cannot satisfy a prior EXPLICIT patch "1.37.4":
	// the two explicit patches would need to match exactly, and one side
	// has none.
	newBare := NewK8sVersionValue("1.37")
	eq, diags = newBare.StringSemanticEquals(context.Background(), NewK8sVersionValue("1.37.4"))
	if diags.HasError() || eq {
		t.Fatalf("a bare new value must not satisfy a prior explicit patch: eq=%v diags=%v", eq, diags)
	}

	// Different minor: never equal, regardless of direction.
	newMinor := NewK8sVersionValue("1.38.0")
	eq, _ = newMinor.StringSemanticEquals(context.Background(), basetypes.NewStringValue("1.37"))
	if eq {
		t.Fatal("1.38.0 vs prior 1.37 must not be equal")
	}

	// Null/unknown on either side falls back to plain structural equality,
	// never to version parsing.
	nullNew := K8sVersionValue{StringValue: basetypes.NewStringNull()}
	eq, diags = nullNew.StringSemanticEquals(context.Background(), K8sVersionValue{StringValue: basetypes.NewStringNull()})
	if diags.HasError() || !eq {
		t.Fatalf("null vs null must be equal: eq=%v diags=%v", eq, diags)
	}

	unknownNew := K8sVersionValue{StringValue: basetypes.NewStringUnknown()}
	eq, diags = unknownNew.StringSemanticEquals(context.Background(), NewK8sVersionValue("1.37"))
	if diags.HasError() || eq {
		t.Fatalf("unknown vs known must not be equal: eq=%v diags=%v", eq, diags)
	}
}

// HasPatch's documented contract: true only for an explicit patch, and an
// unparseable v has no patch. "v1.37.2" is parseable (a leading "v" is
// accepted everywhere in this package), so it has one.
func TestHasPatch(t *testing.T) {
	for _, c := range []struct {
		v    string
		want bool
	}{
		{"1.37", false}, {"1.37.2", true}, {"1.37.0", true}, {"v1.37.2", true}, {"v1.37", false},
		{"", false}, {"1", false}, {"1.37.", false}, {"1.37.x", false}, {"1.37.02", false}, {"bogus", false},
	} {
		if got := HasPatch(c.v); got != c.want {
			t.Errorf("HasPatch(%q) = %v, want %v", c.v, got, c.want)
		}
	}
}
