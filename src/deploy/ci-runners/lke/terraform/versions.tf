terraform {
  required_version = ">= 1.6"
  required_providers {
    linode = {
      source  = "linode/linode"
      version = "~> 2.30"
    }
  }
}

provider "linode" {
  # LINODE_TOKEN from the environment.
}
