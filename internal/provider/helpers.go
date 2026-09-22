package provider

import "github.com/hashicorp/terraform-plugin-framework/types"

// deref returns the string p points at, or "" for nil. For places where the
// API's "absent" and "empty" mean the same thing (a status message); where
// they differ, use optString.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// optString maps an optional API string onto a Terraform value: absent
// (nil, i.e. omitted from the response) becomes null, never "". A computed
// attribute that reads "" where the API said nothing would claim a value the
// API never reported.
func optString(p *string) types.String {
	if p == nil {
		return types.StringNull()
	}
	return types.StringValue(*p)
}
