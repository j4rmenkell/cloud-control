terraform {
  required_version = ">= 1.6"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
    archive = {
      source  = "hashicorp/archive"
      version = "~> 2.4"
    }
  }

  # State is local (gitignored) for now. Milestone 5 moves it to a remote
  # backend so CI can deploy.
}

provider "aws" {
  region  = var.region
  profile = var.aws_profile

  # Every resource is tagged, so the bill can be split by project
  # (Billing → Cost allocation tags) and strays are easy to find.
  default_tags {
    tags = {
      Project   = "cloud-control"
      ManagedBy = "terraform"
      Repo      = "github.com/j4rmenkell/cloud-control"
    }
  }
}
