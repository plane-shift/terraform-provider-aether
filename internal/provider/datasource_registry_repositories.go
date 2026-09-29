package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/apierr"
)

var _ datasource.DataSourceWithConfigure = &registryRepositoriesDataSource{}

// NewRegistryRepositoriesDataSource is aether_registry_repositories (T14-4).
func NewRegistryRepositoriesDataSource() datasource.DataSource {
	return &registryRepositoriesDataSource{}
}

type registryRepositoriesDataSource struct{ pd *providerData }

type repositoryDataModel struct {
	Name          types.String `tfsdk:"name"`
	ArtifactCount types.Int64  `tfsdk:"artifact_count"`
	SizeBytes     types.Int64  `tfsdk:"size_bytes"`
	LastPush      types.String `tfsdk:"last_push"`
}

type registryRepositoriesModel struct {
	Repositories []repositoryDataModel `tfsdk:"repositories"`
}

// harborListWire is the registry list envelope as the API sends it: NOT the
// common ListResponse. next_cursor is ALWAYS present, a Harbor page NUMBER,
// "" on the last page (contract §1/§2). The generated RepositoryList/TagList
// drop it, so the body is decoded here. An ABSENT next_cursor reads as ""
// (last page): a future API that omits it there must not break reads (r1
// N-5). Paging goes through collectAll, so a page number the API has already
// sent is an ERROR, not a stop (T14-15).
type harborListWire[T any] struct {
	Items      *[]T    `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// repositoryWire is one repository as the API sends it. tags_count is
// Harbor's ARTIFACT count; last_push is absent when Harbor reports none.
type repositoryWire struct {
	Name      string     `json:"name"`
	TagsCount int64      `json:"tags_count"`
	SizeBytes int64      `json:"size_bytes"`
	LastPush  *time.Time `json:"last_push"`
}

// decodeHarborPage decodes one registry list page. A body without items is
// not a page; an absent next_cursor means the last page, exactly like "".
func decodeHarborPage[T any](hr *http.Response, status string, body []byte) ([]T, string, diag.Diagnostics) {
	var diags diag.Diagnostics
	var page harborListWire[T]
	if err := json.Unmarshal(body, &page); err != nil || page.Items == nil {
		diags.AddError("Aether API returned no usable registry list", describeUndecodable(hr, status, err, "items"))
		return nil, "", diags
	}
	next := ""
	if page.NextCursor != nil {
		next = *page.NextCursor
	}
	return *page.Items, next, diags
}

func (d *registryRepositoriesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_registry_repositories"
}

func (d *registryRepositoriesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every repository in the organization's registry, sorted by name. " +
			"The list is served live from the registry. A registry that was never enabled answers an error (502); a registry disabled without purge still lists its retained images. " +
			"`size_bytes` is 0 also when the platform could not compute it.",
		Attributes: map[string]schema.Attribute{
			"repositories": schema.ListNestedAttribute{Computed: true,
				MarkdownDescription: "The repositories, sorted by `name`.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"name":           schema.StringAttribute{Computed: true, MarkdownDescription: "Repository name without the project prefix; it can contain `/` (e.g. `team/app`)."},
					"artifact_count": schema.Int64Attribute{Computed: true, MarkdownDescription: "Number of ARTIFACTS (not tags) in the repository."},
					"size_bytes":     schema.Int64Attribute{Computed: true, MarkdownDescription: "Total size of the repository's artifacts, de-duplicated by digest. 0 also when the platform could not compute it."},
					"last_push":      schema.StringAttribute{Computed: true, MarkdownDescription: "Time the repository was last updated (RFC 3339); null when the registry reports none."},
				}}},
		},
	}
}

func (d *registryRepositoriesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *registryRepositoriesDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	repos, err := collectAll(ctx, func(ctx context.Context, cursor string) ([]repositoryWire, string, error) {
		hr, err := d.pd.Client.ListRepositories(ctx, d.pd.orgID, harborPageEditor(cursor))
		if err != nil {
			return nil, "", err
		}
		body, err := readRawBody(hr)
		if err != nil {
			return nil, "", err
		}
		var diags diag.Diagnostics
		if hr.StatusCode != http.StatusOK {
			apierr.ToDiagnostics(apierr.Parse(hr.StatusCode, hr.Header, body), &diags, path.Empty())
			return nil, "", pageFailure(diags)
		}
		items, next, diags := decodeHarborPage[repositoryWire](hr, hr.Status, body)
		if diags.HasError() {
			return nil, "", pageFailure(diags)
		}
		for _, it := range items {
			if it.Name == "" {
				diags.AddError("Aether API returned no usable registry list", describeJSONWithout(hr, hr.Status, "repository name"))
				return nil, "", pageFailure(diags)
			}
		}
		return items, next, nil
	})
	if err != nil {
		addPageError(&resp.Diagnostics, "Cannot list registry repositories", err)
		return
	}
	sort.SliceStable(repos, func(i, j int) bool { return repos[i].Name < repos[j].Name })
	out := registryRepositoriesModel{Repositories: make([]repositoryDataModel, 0, len(repos))}
	for _, r := range repos {
		out.Repositories = append(out.Repositories, repositoryDataModel{
			Name:          types.StringValue(r.Name),
			ArtifactCount: types.Int64Value(r.TagsCount),
			SizeBytes:     types.Int64Value(r.SizeBytes),
			LastPush:      optTimeRFC3339(r.LastPush),
		})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}
