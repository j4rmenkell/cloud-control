variable "region" {
  description = "AWS region for everything cloud-control creates."
  type        = string
  default     = "ap-southeast-1"
}

variable "aws_profile" {
  description = "AWS CLI profile (IAM Identity Center / SSO) used to deploy. Set to null in CI."
  type        = string
  default     = "cloud-control"
}

variable "name" {
  description = "Name prefix for resources."
  type        = string
  default     = "cloud-control"
}

# These identify your Discord app and server. They are not secrets (the
# public key is public by design), but they're kept out of the repo anyway.
# scripts/deploy.sh passes them from .env as TF_VAR_* variables.

variable "discord_public_key" {
  description = "Discord application public key (hex), used to verify interaction signatures."
  type        = string

  validation {
    condition     = can(regex("^[0-9a-fA-F]{64}$", var.discord_public_key))
    error_message = "discord_public_key must be 64 hex characters."
  }
}

variable "owner_discord_user_id" {
  description = "The only Discord user allowed to run commands."
  type        = string
}

variable "discord_guild_id" {
  description = "The Discord server (guild) commands are accepted from."
  type        = string
}

variable "log_retention_days" {
  description = "CloudWatch log retention. Short retention keeps storage inside the free tier."
  type        = number
  default     = 14
}
