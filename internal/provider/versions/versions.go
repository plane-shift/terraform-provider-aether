// Package versions holds the k8s_version custom type: "1.37" and "1.37.0" are
// the same configured intent, so the resolved patch the API reports back is
// never drift.
//
// There is deliberately NO "latest" keyword at this layer. A version string
// here is a version; resolving a moving target belongs to whatever chooses the
// value, not to the type that compares two of them.
package versions

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// A leading "v" is accepted because practitioners write both; no other
// decoration is. Leading zeros are rejected so "1.07" cannot silently mean 1.7.
//
// Each component is bounded to 9 digits, and that bound is load-bearing rather
// than cosmetic. strconv.Atoi does NOT return 0 on overflow: it CLAMPS to
// math.MaxInt64 and reports strconv.ErrRange alongside. With an unbounded
// `\d*` and a discarded error, every over-long component collapsed to the same
// MaxInt64 sentinel, so "99999999999999999999.37" and "88888888888888888888.37"
// compared EQUAL. Nine digits cannot overflow an int, so the clamp is now
// unreachable; parse also propagates the error, so an unbounded component is
// rejected at the regex rather than silently normalised.
var re = regexp.MustCompile(`^v?(0|[1-9]\d{0,8})\.(0|[1-9]\d{0,8})(?:\.(0|[1-9]\d{0,8}))?$`)

type parsed struct {
	major, minor, patch int
	hasPatch            bool
}

func parse(s string) (parsed, error) {
	m := re.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return parsed{}, fmt.Errorf("%q is not MAJOR.MINOR or MAJOR.MINOR.PATCH", s)
	}
	var p parsed
	var err error
	// Unreachable while the regex bounds each component to 9 digits - which is
	// exactly why the error is returned rather than discarded: if that bound is
	// ever relaxed, this fails loudly instead of silently clamping two
	// different versions onto one sentinel.
	if p.major, err = strconv.Atoi(m[1]); err != nil {
		return parsed{}, fmt.Errorf("major component of %q: %w", s, err)
	}
	if p.minor, err = strconv.Atoi(m[2]); err != nil {
		return parsed{}, fmt.Errorf("minor component of %q: %w", s, err)
	}
	if m[3] != "" {
		if p.patch, err = strconv.Atoi(m[3]); err != nil {
			return parsed{}, fmt.Errorf("patch component of %q: %w", s, err)
		}
		p.hasPatch = true
	}
	return p, nil
}

// Minor returns the major and minor components of v.
func Minor(v string) (int, int, error) {
	p, err := parse(v)
	return p.major, p.minor, err
}

// SemanticallyEqual reports whether an actual version satisfies a configured
// one. A configured version without a patch matches ANY patch of that minor;
// two explicit patches must match exactly.
//
// It returns false - never an error - when either side does not parse. This is
// a plan-time equality hook: an unparseable value is simply not equal, and the
// diagnostic that explains why belongs to validation, not here.
func SemanticallyEqual(configured, actual string) bool {
	// Empty equals only empty, and is never fed to the parser: an absent value
	// is not a malformed one.
	if configured == "" || actual == "" {
		return configured == actual
	}
	c, cErr := parse(configured)
	a, aErr := parse(actual)
	if cErr != nil || aErr != nil {
		return false
	}
	if c.major != a.major || c.minor != a.minor {
		return false
	}
	if !c.hasPatch {
		return true
	}
	return a.hasPatch && c.patch == a.patch
}

// IsForward reports whether to is a newer version than from (major, then minor,
// then patch). Identical versions are NOT forward, so an in-place "upgrade" to
// the version already running is not mistaken for one.
func IsForward(from, to string) (bool, error) {
	f, err := parse(from)
	if err != nil {
		return false, err
	}
	t, err := parse(to)
	if err != nil {
		return false, err
	}
	if t.major != f.major {
		return t.major > f.major, nil
	}
	if t.minor != f.minor {
		return t.minor > f.minor, nil
	}
	// Same minor: only two EXPLICIT patches can be ordered. "1.37" against
	// "1.37.4" states no patch intent, so it is not a forward move.
	return t.hasPatch && f.hasPatch && t.patch > f.patch, nil
}

// ---- custom type plumbing --------------------------------------------------

var _ basetypes.StringTypable = K8sVersionType{}

// K8sVersionType is the attr.Type of a k8s_version attribute.
type K8sVersionType struct{ basetypes.StringType }

func (K8sVersionType) Equal(o attr.Type) bool { _, ok := o.(K8sVersionType); return ok }

func (K8sVersionType) String() string { return "K8sVersionType" }

func (t K8sVersionType) ValueFromString(_ context.Context, in basetypes.StringValue) (basetypes.StringValuable, diag.Diagnostics) {
	return K8sVersionValue{StringValue: in}, nil
}

func (t K8sVersionType) ValueFromTerraform(ctx context.Context, in tftypes.Value) (attr.Value, error) {
	v, err := t.StringType.ValueFromTerraform(ctx, in)
	if err != nil {
		return nil, err
	}
	sv, ok := v.(basetypes.StringValue)
	if !ok {
		return nil, fmt.Errorf("unexpected value type %T from StringType", v)
	}
	return K8sVersionValue{StringValue: sv}, nil
}

func (K8sVersionType) ValueType(context.Context) attr.Value { return K8sVersionValue{} }

var _ basetypes.StringValuableWithSemanticEquals = K8sVersionValue{}

// K8sVersionValue is a string value whose equality is version equality.
type K8sVersionValue struct{ basetypes.StringValue }

// NewK8sVersionValue returns a known K8sVersionValue holding s.
func NewK8sVersionValue(s string) K8sVersionValue {
	return K8sVersionValue{StringValue: basetypes.NewStringValue(s)}
}

func (v K8sVersionValue) Type(context.Context) attr.Type { return K8sVersionType{} }

func (v K8sVersionValue) Equal(o attr.Value) bool {
	ov, ok := o.(K8sVersionValue)
	return ok && v.StringValue.Equal(ov.StringValue)
}

// StringSemanticEquals stops "1.37" in configuration from reading as drift
// against the "1.37.4" the API resolved.
//
// It runs when the framework writes data into state - after Create, Update and
// Read - not during planning.
//
// DIRECTION MATTERS, and the two orders are different functions. The framework
// calls `proposedNew.StringSemanticEquals(ctx, prior)` and, on true, keeps the
// PRIOR value in state (terraform-plugin-framework v1.19.0,
// internal/fwschemadata/value_semantic_equality_string.go:37 and :53). So the
// RECEIVER is the new value - what the API just produced - and the ARGUMENT is
// the prior, configured one. SemanticallyEqual takes (configured, actual), so
// the argument goes first: SemanticallyEqual("1.37", "1.37.4") is true while
// SemanticallyEqual("1.37.4", "1.37") is false.
func (v K8sVersionValue) StringSemanticEquals(ctx context.Context, newValuable basetypes.StringValuable) (bool, diag.Diagnostics) {
	var diags diag.Diagnostics
	nv, d := newValuable.ToStringValue(ctx)
	diags.Append(d...)
	if diags.HasError() {
		return false, diags
	}
	// Null and unknown carry no version to compare. Fall back to plain string
	// equality so unknown-vs-unknown and null-vs-null keep their ordinary
	// framework meaning instead of being parsed as versions.
	if v.IsNull() || v.IsUnknown() || nv.IsNull() || nv.IsUnknown() {
		return v.StringValue.Equal(nv), diags
	}
	// nv = the ARGUMENT = prior/configured; v = the RECEIVER = the new value.
	return SemanticallyEqual(nv.ValueString(), v.ValueString()), diags
}
