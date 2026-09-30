terraform {
  required_version = ">= 1.5" # OpenTofu >= 1.6 works the same way
  required_providers {
    aether = {
      source  = "plane-shift/aether"
      version = "~> 0.1"
    }
  }
}

variable "aether_api_token" {
  type      = string
  sensitive = true
  default   = null # unset here so the provider falls back to AETHER_API_TOKEN
}

# Create an API token in the Aether portal first (Settings → API Tokens).
provider "aether" {
  api_token = var.aether_api_token # or AETHER_API_TOKEN
  # endpoint defaults to https://api.aetherplatform.cloud (production);
  # use https://dev.api.aetherplatform.cloud for the dev environment (or AETHER_ENDPOINT).
}
