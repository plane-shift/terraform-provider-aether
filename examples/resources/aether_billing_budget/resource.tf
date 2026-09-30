# Advisory only: nothing is blocked. One usage.alert webhook per threshold per UTC month.
resource "aether_billing_budget" "this" {
  monthly_limit_eur = 500
  thresholds_pct    = [50, 80, 100]
}
