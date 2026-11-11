# ---- Package: the Go binary (bootstrap) + config.yaml, built by scripts/build-lambda.sh

data "archive_file" "lambda" {
  type        = "zip"
  source_dir  = "${path.module}/../../build"
  output_path = "${path.module}/.terraform/lambda.zip"
}

# ---- Logs: created explicitly so retention is set (Lambda's default is "never expire")

resource "aws_cloudwatch_log_group" "lambda" {
  name              = "/aws/lambda/${var.name}"
  retention_in_days = var.log_retention_days
}

# ---- IAM: the function may only write its own logs. Nothing else, yet.

data "aws_iam_policy_document" "assume_lambda" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "lambda" {
  name               = "${var.name}-lambda"
  assume_role_policy = data.aws_iam_policy_document.assume_lambda.json
}

data "aws_iam_policy_document" "lambda" {
  statement {
    sid       = "WriteOwnLogs"
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["${aws_cloudwatch_log_group.lambda.arn}:*"]
  }
}

resource "aws_iam_role_policy" "lambda" {
  name   = "least-privilege"
  role   = aws_iam_role.lambda.id
  policy = data.aws_iam_policy_document.lambda.json
}

# ---- Function

resource "aws_lambda_function" "this" {
  function_name    = var.name
  description      = "cloud-control: Discord interactions endpoint"
  role             = aws_iam_role.lambda.arn
  runtime          = "provided.al2023"
  handler          = "bootstrap"
  architectures    = ["arm64"] # Graviton: cheaper per GB-s than x86 once past the free tier
  filename         = data.archive_file.lambda.output_path
  source_code_hash = data.archive_file.lambda.output_base64sha256
  memory_size      = 128
  timeout          = 10

  # No reserved concurrency: new accounts have a 10-execution account limit
  # and AWS requires all 10 to stay unreserved. That account limit already
  # bounds how many copies can run at once.

  environment {
    variables = {
      CONFIG_PATH           = "config.yaml"
      DISCORD_PUBLIC_KEY    = var.discord_public_key
      OWNER_DISCORD_USER_ID = var.owner_discord_user_id
      DISCORD_GUILD_ID      = var.discord_guild_id
    }
  }

  depends_on = [aws_cloudwatch_log_group.lambda, aws_iam_role_policy.lambda]
}

# ---- Public HTTPS endpoint for Discord. Auth is the Ed25519 signature check in code.

# With authorization_type NONE, the AWS provider itself adds the two
# resource-policy statements a public URL needs (AWS tightened this in late
# 2025): lambda:InvokeFunctionUrl, and lambda:InvokeFunction limited to calls
# made *through* the URL. Declaring them again here duplicates them and races
# the provider (409 ResourceConflictException), so they are not repeated.
resource "aws_lambda_function_url" "this" {
  function_name      = aws_lambda_function.this.function_name
  authorization_type = "NONE"
}
