package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/apierr"
)

var _ datasource.DataSourceWithConfigure = &registryRepositoryTagsDataSource{}

// NewRegistryRepositoryTagsDataSource is aether_registry_repository_tags (T14-5).
func NewRegistryRepositoryTagsDataSource() datasource.DataSource {
	return &registryRepositoryTagsDataSource{}
}

type registryRepositoryTagsDataSource struct{ pd *providerData }

type tagVulnerabilitiesModel struct {
	Critical types.Int64 `tfsdk:"critical"`
	High     types.Int64 `tfsdk:"high"`
	Medium   types.Int64 `tfsdk:"medium"`
	Low      types.Int64 `tfsdk:"low"`
	Unknown  types.Int64 `tfsdk:"unknown"`
}

type tagDataModel struct {
	Name      types.String `tfsdk:"name"`
	Digest    types.String `tfsdk:"digest"`
	SizeBytes types.Int64  `tfsdk:"size_bytes"`
	PushedAt  types.String `tfsdk:"pushed_at"`
	PulledAt  types.String `tfsdk:"pulled_at"`
	Untagged  types.Bool   `tfsdk:"untagged"`
	// nil = not successfully scanned → a null object, distinct from zeros.
	Vulnerabilities *tagVulnerabilitiesModel `tfsdk:"vulnerabilities"`
}

type registryRepositoryTagsModel struct {
	Repository types.String   `tfsdk:"repository"`
	Tags       []tagDataModel `tfsdk:"tags"`
}

// tagWire is one tag item as the API sends it. The generated Tag has no
// vulnerabilities, its VulnSummary lacks low/unknown and its times are not
// pointers (an absent time would read 0001-01-01), so the body is decoded
// here (contract §2).
type tagWire struct {
	Name            string     `json:"name"`
	Digest          string     `json:"digest"`
	SizeBytes       int64      `json:"size_bytes"`
	PushedAt        *time.Time `json:"pushed_at"`
	PulledAt        *time.Time `json:"pulled_at"`
	Untagged        bool       `json:"untagged"`
	Vulnerabilities *struct {
		Critical int64 `json:"critical"`
		High     int64 `json:"high"`
		Medium   int64 `json:"medium"`
		Low      int64 `json:"low"`
		Unknown  int64 `json:"unknown"`
	} `json:"vulnerabilities"`
}

// errRepositoryNotFound marks the tags route's 404 so Read can name the
// repository attribute.
var errRepositoryNotFound = errors.New("repository not found")

func (d *registryRepositoryTagsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_registry_repository_tags"
}

func (d *registryRepositoryTagsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The tags of one repository in the organization's registry, served live from the registry. " +
			"There is one entry per artifact and tag: an artifact with several tags appears once per tag (the same `digest` repeats), and an untagged artifact appears once with an empty `name`. " +
			"Entries are sorted by `digest`, then `name`. A `null` `vulnerabilities` means the artifact has not been successfully scanned — distinct from zero findings. " +
			"A repository that does not exist, or a registry that was never enabled, is an error.",
		Attributes: map[string]schema.Attribute{
			"repository": schema.StringAttribute{Required: true,
				MarkdownDescription: "Repository name exactly as `aether_registry_repositories` returns it (plain, not URL-encoded; it can contain `/`, e.g. `team/app`).",
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)}},
			"tags": schema.ListNestedAttribute{Computed: true,
				MarkdownDescription: "One entry per artifact and tag, sorted by `digest`, then `name`.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"name":       schema.StringAttribute{Computed: true, MarkdownDescription: "Tag name; empty for an untagged artifact."},
					"digest":     schema.StringAttribute{Computed: true, MarkdownDescription: "Artifact digest. Not unique across entries."},
					"size_bytes": schema.Int64Attribute{Computed: true, MarkdownDescription: "Artifact size in bytes."},
					"pushed_at":  schema.StringAttribute{Computed: true, MarkdownDescription: "Push time (RFC 3339); null when the registry reports none."},
					"pulled_at":  schema.StringAttribute{Computed: true, MarkdownDescription: "Last pull time (RFC 3339); null when the artifact was never pulled."},
					"untagged":   schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the artifact has no tag."},
					"vulnerabilities": schema.SingleNestedAttribute{Computed: true,
						MarkdownDescription: "Vulnerability counts of the last successful scan; null when the artifact has not been successfully scanned (not scanned, scan running or failed) — distinct from all zeros.",
						Attributes: map[string]schema.Attribute{
							"critical": schema.Int64Attribute{Computed: true},
							"high":     schema.Int64Attribute{Computed: true},
							"medium":   schema.Int64Attribute{Computed: true},
							"low":      schema.Int64Attribute{Computed: true},
							"unknown":  schema.Int64Attribute{Computed: true},
						}},
				}}},
		},
	}
}

func (d *registryRepositoryTagsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *registryRepositoryTagsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg registryRepositoryTagsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// The plain name: the generated client path-escapes it once (team/app →
	// team%2Fapp); pre-escaping here would double it (contract §2).
	repo := cfg.Repository.ValueString()
	var requestID string
	tags, err := collectAll(ctx, func(ctx context.Context, cursor string) ([]tagWire, string, error) {
		res, err := d.pd.Client.ListRepositoryTagsWithResponse(ctx, d.pd.orgID, repo, harborPageEditor(cursor))
		if err != nil {
			return nil, "", err
		}
		var diags diag.Diagnostics
		if res.StatusCode() == http.StatusNotFound {
			var ae *apierr.Error
			if errors.As(apierr.Parse(res.StatusCode(), res.HTTPResponse.Header, res.Body), &ae) {
				requestID = ae.RequestID
			}
			return nil, "", errRepositoryNotFound
		}
		if res.StatusCode() != http.StatusOK {
			apierr.ToDiagnostics(apierr.Parse(res.StatusCode(), res.HTTPResponse.Header, res.Body), &diags, path.Empty())
			return nil, "", pageFailure(diags)
		}
		items, next, diags := decodeHarborPage[tagWire](res.HTTPResponse, res.Status(), res.Body)
		if diags.HasError() {
			return nil, "", pageFailure(diags)
		}
		for _, it := range items {
			if it.Digest == "" {
				diags.AddError("Aether API returned no usable registry list", describeJSONWithout(res.HTTPResponse, res.Status(), "tag digest"))
				return nil, "", pageFailure(diags)
			}
		}
		return items, next, nil
	})
	if errors.Is(err, errRepositoryNotFound) {
		rid := ""
		if requestID != "" {
			rid = " (request id " + requestID + ")"
		}
		resp.Diagnostics.AddAttributeError(path.Root("repository"), "Repository not found",
			fmt.Sprintf("%q: repository not found (or the registry is not enabled)%s. Use the plain name aether_registry_repositories returns, e.g. \"team/app\".", repo, rid))
		return
	}
	if err != nil {
		addPageError(&resp.Diagnostics, "Cannot list repository tags", err)
		return
	}
	sort.SliceStable(tags, func(i, j int) bool {
		if tags[i].Digest != tags[j].Digest {
			return tags[i].Digest < tags[j].Digest
		}
		return tags[i].Name < tags[j].Name
	})
	cfg.Tags = make([]tagDataModel, 0, len(tags))
	for _, t := range tags {
		m := tagDataModel{
			Name:      types.StringValue(t.Name),
			Digest:    types.StringValue(t.Digest),
			SizeBytes: types.Int64Value(t.SizeBytes),
			PushedAt:  optTimeRFC3339(t.PushedAt),
			PulledAt:  optTimeRFC3339(t.PulledAt),
			Untagged:  types.BoolValue(t.Untagged),
		}
		if v := t.Vulnerabilities; v != nil {
			m.Vulnerabilities = &tagVulnerabilitiesModel{
				Critical: types.Int64Value(v.Critical), High: types.Int64Value(v.High), Medium: types.Int64Value(v.Medium),
				Low: types.Int64Value(v.Low), Unknown: types.Int64Value(v.Unknown),
			}
		}
		cfg.Tags = append(cfg.Tags, m)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &cfg)...)
}
