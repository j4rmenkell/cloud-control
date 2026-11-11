output "interactions_endpoint_url" {
  description = "Paste into Discord Developer Portal → General Information → Interactions Endpoint URL."
  value       = aws_lambda_function_url.this.function_url
}

output "function_name" {
  value = aws_lambda_function.this.function_name
}

output "log_group" {
  description = "aws logs tail <this> --follow --profile cloud-control"
  value       = aws_cloudwatch_log_group.lambda.name
}
