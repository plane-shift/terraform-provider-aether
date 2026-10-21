package provider

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/client"
	"github.com/plane-shift/terraform-provider-aether/internal/provider/apierr"
	"github.com/plane-shift/terraform-provider-aether/internal/provider/transport"
)

const defaultEndpoint = "https://api.aetherplatform.cloud"

// providerData is what every resource and data source receives from Configure.
type providerData struct {
	Client *client.ClientWithResponses
	// OrgID is canonical UUID TEXT. Resource models carry ids as strings, so
	// this is the form they copy into state.
	OrgID string
	// orgID is the same id in the form the generated client demands
	// (client.OrgID = openapi_types.UUID). Parsed ONCE here, on whichever
	// path resolved it, so no resource re-parses a value Configure has
	// already validated and no call site needs error handling for it. Ids
	// that arrive from STATE are a different case and are parsed per call:
	// state is data from outside this process.
	orgID client.OrgID
	// ownerDeleteMu serialises the last-owner guard + DELETE of owner
	// memberships (aether_organization_member). One provider process serves
	// one Terraform run, so this stops parallel owner deletes in a run from
	// each seeing the other as "another owner"; it cannot reach the portal
	// or another run (the API refuses that case itself with 409 last_owner
	// where it enforces organization roles). providerData is
	// shared by pointer and never copied.
	ownerDeleteMu sync.Mutex
}

type aetherProvider struct{ version string }

type providerModel struct {
	APIToken       types.String `tfsdk:"api_token"`
	Endpoint       types.String `tfsdk:"endpoint"`
	OrganizationID types.String `tfsdk:"organization_id"`
}

func New(version string) func() provider.Provider {
	return func() provider.Provider { return &aetherProvider{version: version} }
}

func (p *aetherProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "aether"
	resp.Version = p.version
}

func (p *aetherProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manage Aether Kubernetes clusters, access and registry. Create an API token in the Aether portal first; the token belongs to one organization, which the provider uses for every resource.",
		Attributes: map[string]schema.Attribute{
			"api_token": schema.StringAttribute{
				Optional: true, Sensitive: true,
				MarkdownDescription: "API token created in the portal (`aether_…`). Falls back to `AETHER_API_TOKEN`.",
			},
			"endpoint": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "API base URL. Defaults to `" + defaultEndpoint + "`; falls back to `AETHER_ENDPOINT`. Must be `https`, or `http` with a loopback host (`localhost` or a loopback IP) — the API token is sent on every request, so plain `http` to any other host is refused rather than leaking the credential in cleartext.",
			},
			"organization_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Organization the token belongs to. Optional: resolved automatically when the token sees exactly one organization. Falls back to `AETHER_ORGANIZATION_ID`.",
			},
		},
	}
}

func (p *aetherProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// An unknown value is not an absent one, and ValueString() flattens the
	// difference to "". Left unchecked, an unknown api_token is reported as
	// "missing" (wrong: it is not yet known) and an unknown organization_id
	// falls through to auto-resolution, reaching the API before the value the
	// practitioner configured even exists. Checked BEFORE the env fallback,
	// because falling back to the environment would silently use a different
	// organization than the one being configured.
	for _, u := range []struct {
		attr  string
		value types.String
		env   string
	}{
		{"api_token", cfg.APIToken, "AETHER_API_TOKEN"},
		{"endpoint", cfg.Endpoint, "AETHER_ENDPOINT"},
		{"organization_id", cfg.OrganizationID, "AETHER_ORGANIZATION_ID"},
	} {
		if u.value.IsUnknown() {
			resp.Diagnostics.AddAttributeError(path.Root(u.attr),
				"Unknown configuration value for "+u.attr,
				fmt.Sprintf("The provider cannot create the Aether API client: %s is not yet known at configure time. "+
					"Either target apply the source of the value first, set the value statically, or use the %s environment variable.", u.attr, u.env))
		}
	}
	if resp.Diagnostics.HasError() {
		return
	}

	token := firstNonEmpty(cfg.APIToken.ValueString(), os.Getenv("AETHER_API_TOKEN"))
	endpoint := firstNonEmpty(cfg.Endpoint.ValueString(), os.Getenv("AETHER_ENDPOINT"), defaultEndpoint)
	orgID := firstNonEmpty(cfg.OrganizationID.ValueString(), os.Getenv("AETHER_ORGANIZATION_ID"))
	if token == "" {
		resp.Diagnostics.AddAttributeError(path.Root("api_token"), "Missing API token",
			"Set api_token in the provider block or the AETHER_API_TOKEN environment variable. Tokens are created in the Aether portal.")
		return
	}

	// Check the endpoint BEFORE anything is sent to it. The bearer token goes
	// on every request, so a plain-http endpoint puts a long-lived credential
	// on the wire in cleartext - and it is the kind of thing that gets set
	// once while debugging and then committed.
	endpointURL, err := url.Parse(endpoint)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("endpoint"), "Invalid endpoint",
			fmt.Sprintf("endpoint %q is not a valid URL: %s", endpoint, err))
		return
	}
	if endpointURL.Hostname() == "" {
		// Separated from the insecure-scheme case: telling someone their
		// endpoint is insecure when what it actually lacks is a host sends
		// them to fix the wrong half of the string.
		resp.Diagnostics.AddAttributeError(path.Root("endpoint"), "Endpoint has no host",
			fmt.Sprintf("endpoint %q has no host. Give a full URL such as %s — note that %q parses as a URL but names no host to connect to.", endpoint, defaultEndpoint, endpoint))
		return
	}
	if !endpointIsSafe(endpointURL) {
		resp.Diagnostics.AddAttributeError(path.Root("endpoint"), "Insecure endpoint",
			fmt.Sprintf("endpoint %q would send the API token in cleartext; plain http is accepted only for loopback endpoints (localhost or a loopback IP). Use https, or a loopback address when testing against a local API.", endpoint))
		return
	}

	// The generated client types every organization id as a UUID, so an id that
	// is not one cannot even be turned into a request path. Parse it HERE, at
	// the boundary, before any call: a syntax error belongs on the
	// organization_id attribute, not in a downstream encoding failure whose
	// message names neither the attribute nor the value.
	//
	// providerData.OrgID stays a string - canonical UUID text - because every
	// resource carries it around as configuration data; the parsed form rides
	// alongside it as providerData.orgID, so the parse happens exactly once
	// per Configure no matter which path below resolved the id.
	var orgUUID client.OrgID
	if orgID != "" {
		if err := orgUUID.UnmarshalText([]byte(orgID)); err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("organization_id"), "Invalid organization_id",
				fmt.Sprintf("organization_id must be a UUID: %q is not (%s). Copy the id from the Aether portal, or omit organization_id and the provider resolves the organization the token belongs to.", orgID, err))
			return
		}
		// Canonicalise, so state holds the same text the API round-trips.
		orgID = orgUUID.String()
	}

	httpClient := &http.Client{
		Transport: transport.New(token, apiTransport()),
		// An API client has no business following a 302, and here it would be
		// a credential leak rather than merely wasted work: the retrying
		// transport re-sets the bearer header on EVERY request it sees,
		// including a redirected one to another host. The stdlib's own
		// protection does not cover this - it strips Authorization only
		// across different HOSTNAMES, ignoring the port, and it cannot strip
		// a header a RoundTripper adds afterwards.
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			return fmt.Errorf("refusing to follow a redirect to %s: the Aether API does not redirect, and following one would send the API token to that host", req.URL.Redacted())
		},
	}
	c, err := client.NewClientWithResponses(endpoint, client.WithHTTPClient(httpClient))
	if err != nil {
		resp.Diagnostics.AddError("Cannot build API client", err.Error())
		return
	}

	if orgID != "" {
		// Validate the override: the token belongs to exactly one organization.
		g, err := c.GetOrganizationWithResponse(ctx, orgUUID)
		if err != nil {
			resp.Diagnostics.AddError("Cannot reach the Aether API", fmt.Sprintf("GET %s/api/v1/organizations/%s: %s", endpoint, orgID, err))
			return
		}
		// Three different causes, three different diagnostics. One fixed
		// message blamed organization_id for a rejected token and for a
		// transient server failure alike, sending the practitioner to edit a
		// value that was never the problem.
		switch {
		case g.StatusCode() == http.StatusOK:
		case g.StatusCode() == http.StatusUnauthorized || g.StatusCode() == http.StatusForbidden:
			resp.Diagnostics.AddError("Aether API rejected the token",
				fmt.Sprintf("The API answered %s. Check api_token (created in the portal): this status means the TOKEN was rejected, not that the organization is unknown.", g.Status()))
			return
		case g.StatusCode() == http.StatusNotFound:
			resp.Diagnostics.AddAttributeError(path.Root("organization_id"), "organization_id is not visible to this token",
				fmt.Sprintf("The API answered %s for organization %s. The token belongs to one organization; omit organization_id to use it.", g.Status(), orgID))
			return
		default:
			resp.Diagnostics.AddError("Cannot validate the organization",
				fmt.Sprintf("The Aether API answered %s for GET %s/api/v1/organizations/%s. That is neither a rejected token nor an unknown organization, so nothing in the provider block needs changing; retry, or check the API's status.",
					g.Status(), endpoint, orgID))
			return
		}
		// The generated parser fills JSON200 only when the response carries a
		// JSON content type. A 200 of HTML is a captive portal or a proxy
		// error page, not a confirmation that this organization exists.
		if g.JSON200 == nil {
			resp.Diagnostics.AddError("Aether API answered 200 without a JSON body",
				fmt.Sprintf("GET %s/api/v1/organizations/%s returned %s with content type %q. Check that endpoint points at the Aether API and not at a proxy or login page.",
					endpoint, orgID, g.Status(), g.HTTPResponse.Header.Get("Content-Type")))
			return
		}
	} else {
		list, err := c.ListOrganizationsWithResponse(ctx, nil)
		if err != nil {
			resp.Diagnostics.AddError("Cannot reach the Aether API", fmt.Sprintf("GET %s/api/v1/organizations: %s", endpoint, err))
			return
		}
		switch list.StatusCode() {
		case http.StatusOK:
		case http.StatusUnauthorized, http.StatusForbidden:
			resp.Diagnostics.AddError("Aether API rejected the token",
				"The API answered "+list.Status()+". Check api_token (created in the portal) and, if set, organization_id.")
			return
		default:
			// Bounded and UTF-8-sanitised: the body here is whatever the far
			// end sent, and an unbounded error page in a diagnostic is both
			// unreadable and a protocol hazard.
			resp.Diagnostics.AddError("Cannot list organizations",
				list.Status()+": "+apierr.Truncate(string(list.Body), 200))
			return
		}
		// Same guard on the auto-resolution path: without it a 200 carrying
		// HTML dereferences a nil JSON200 and panics the provider.
		if list.JSON200 == nil {
			resp.Diagnostics.AddError("Aether API answered 200 without a JSON body",
				fmt.Sprintf("GET %s/api/v1/organizations returned %s with content type %q. Check that endpoint points at the Aether API and not at a proxy or login page.",
					endpoint, list.Status(), list.HTTPResponse.Header.Get("Content-Type")))
			return
		}
		items := list.JSON200.Items
		if len(items) != 1 {
			resp.Diagnostics.AddAttributeError(path.Root("organization_id"), "Cannot resolve the organization",
				fmt.Sprintf("The token sees %d organizations; set organization_id explicitly.", len(items)))
			return
		}
		// Parse once, here, like the explicit path above: the client wants the
		// UUID, the models want the text, and both come from this one value.
		orgUUID = items[0].Id
		orgID = orgUUID.String()
	}

	data := &providerData{Client: c, OrgID: orgID, orgID: orgUUID}
	resp.ResourceData = data
	resp.DataSourceData = data
}

func (p *aetherProvider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{NewClusterResource, NewNodePoolResource, NewClusterNetworkResource,
		NewOrganizationResource, NewOrganizationMemberResource, NewWebhookResource,
		NewRegistryResource, NewRegistryRobotAccountResource, NewRegistryCleanupPolicyResource,
		NewBillingBudgetResource}
}

func (p *aetherProvider) DataSources(context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		// Task 13 — cluster-scoped
		NewOrganizationDataSource, NewRegionsDataSource, NewK8sVersionsDataSource,
		NewClusterDataSource, NewClustersDataSource, NewNodePoolDataSource, NewNodePoolsDataSource,
		NewNodePoolMachinesDataSource, NewClusterNetworkDataSource, NewKubeconfigDataSource,
		NewLoadBalancersDataSource, NewClusterVolumesDataSource,
		// Task 14 — organization / registry / billing
		NewOrganizationMembersDataSource,
		NewWebhooksDataSource,
		NewRegistryDataSource,
		NewRegistryRepositoriesDataSource,
		NewRegistryRepositoryTagsDataSource,
		NewRegistryRobotAccountsDataSource,
		NewRegistryCleanupPoliciesDataSource,
		NewAuditLogDataSource,
		NewUsageDataSource,
		NewCostBreakdownDataSource,
		NewInvoicesDataSource,
	}
}

// endpointIsSafe reports whether the token may be sent to this endpoint.
// https always; http only when the traffic cannot leave the machine. A missing
// or unknown scheme is refused too - "api.example.com" parses as a relative
// URL with no scheme, which is a configuration mistake worth naming rather
// than silently defaulting.
//
// A HOSTLESS url is refused for both schemes. url.Parse is permissive: it
// accepts "https:///api" (authority present but empty) and
// "https:api.example.com" (an OPAQUE url, where the host ends up in u.Opaque
// and u.Host stays empty). Both used to pass this check and then failed much
// later as "Cannot reach the Aether API", which names neither the attribute
// nor the typo that caused it.
func endpointIsSafe(u *url.URL) bool {
	if u.Hostname() == "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	default:
		return false
	}
}

// apiTransport is http.DefaultTransport with a bound on how long a host may
// hold the connection open without answering. Without it, a host that
// completes the TCP/TLS handshake and then sends nothing hangs terraform plan
// forever: DefaultTransport bounds dialling and TLS, not the wait for response
// headers.
//
// Deliberately NOT http.Client.Timeout, which would also bound the body and
// so cap every long-running call; waiting for a slow operation is the wait
// package's job, with its own timeouts.
func apiTransport() http.RoundTripper {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// Only reachable if something replaced DefaultTransport. Use it as-is
		// rather than silently dropping whatever it does.
		return http.DefaultTransport
	}
	t := base.Clone()
	t.ResponseHeaderTimeout = 60 * time.Second
	return t
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// fromProviderData is the shared Configure body of every resource/data source.
func fromProviderData(raw any, diags interface{ AddError(string, string) }) *providerData {
	if raw == nil {
		return nil
	}
	pd, ok := raw.(*providerData)
	if !ok {
		diags.AddError("Unexpected provider data", fmt.Sprintf("got %T", raw))
		return nil
	}
	return pd
}
