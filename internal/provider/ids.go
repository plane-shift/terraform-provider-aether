package provider

import (
	"fmt"

	"github.com/plane-shift/terraform-provider-aether/internal/client"
)

// parseID turns an id the provider holds as canonical UUID TEXT into the
// openapi_types.UUID the generated client requires. client.OrgID, ClusterID,
// PoolID and friends are all ALIASES of that one type, so a single helper
// serves every call.
//
// The organization id is parsed ONCE, in Configure, and carried on
// providerData as orgID - resource code never re-parses it. Ids that arrive
// from STATE (cluster, pool) are parsed per call, because state is data from
// outside this process: a hand-edited or truncated id must become an
// attribute diagnostic, not a request that cannot be built.
func parseID(s string) (client.OrgID, error) {
	var u client.OrgID
	if err := u.UnmarshalText([]byte(s)); err != nil {
		return u, fmt.Errorf("%q is not a UUID: %w", s, err)
	}
	return u, nil
}
