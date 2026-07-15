terraform {
  required_version = ">= 1.5.0"

  required_providers {
    # helm/kubernetes/kubectl are CONFIGURED IN THE ROOT (providers.tf, from the
    # cluster workspace's remote-state kubeconfig) and passed in via the module
    # block's `providers = { ... }` map — hence configuration_aliases here rather
    # than a provider config inside the module.
    helm = {
      source                = "hashicorp/helm"
      version               = "~> 2.0"
      configuration_aliases = [helm]
    }
    kubernetes = {
      source                = "hashicorp/kubernetes"
      version               = "~> 2.0"
      configuration_aliases = [kubernetes]
    }
    kubectl = {
      source                = "alekc/kubectl"
      version               = "~> 2.0"
      configuration_aliases = [kubectl]
    }
    # local/null need no configuration (default provider inherited from the root).
    local = {
      source  = "hashicorp/local"
      version = "~> 2.4"
    }
    null = {
      source  = "hashicorp/null"
      version = "~> 3.2"
    }
  }
}
