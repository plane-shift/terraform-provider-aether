# Changelog

Release notes for the GitHub Release are written from this file; GoReleaser's
generated changelog is disabled (`.goreleaser.yml`).

## 0.1.1 (Unreleased)

The Aether API now enforces organization roles on every request. No resource,
data source or attribute changes; the provider keeps calling the same routes.

ENHANCEMENTS:

* resource/aether_cluster: `capi_cluster_name` (also on `data.aether_cluster`) is described as the suffixed name the API gives clusters created after 2026-10, `<organization slug>-<name>-<6 random lowercase alphanumerics>`; earlier clusters keep `<organization slug>-<name>`. The API now refuses a new cluster whose organization slug plus name exceeds 22 characters with `422 validation_error` on `name`; existing clusters are unaffected and a rename is not re-checked. Descriptions only, no schema change.
* provider: an API refusal with `403 insufficient_role` is reported as
  `The API token's organization role does not allow this operation`, and
  `403 token_not_permitted` as `API tokens cannot perform this operation`. The
  detail carries the API message and the request id. Other `401`/`403` answers
  keep the existing `Aether API rejected the request` diagnostic.
* resource/aether_organization_member: the API's `409 last_owner` on a member
  removal is reported as `Cannot remove the organization's last owner`. The
  provider's own last-owner check still runs before any removal is sent; its
  diagnostic now uses the same summary.

NOTES:

* Only an organization owner can create API tokens (portal, Settings → API
  Tokens). An organization token acts with the `admin` role on everything the
  provider manages, may add members with any role (`owner` included) and may
  remove owners except the last one.
* resource/aether_organization_member: member emails are compared
  case-insensitively by the API; adding an address that differs from an
  existing member's only in letter case is refused with `409`. Emails must
  still be written in lower case in configuration.
* An API token is not tied to the member who created it. After removing an
  owner, revoke the API tokens they created.

## 0.1.0 (2026-10-12)

First public release on the Terraform Registry and the OpenTofu Registry as
`plane-shift/aether`.
