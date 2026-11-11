// Command lambda is cloud-control's AWS Lambda entrypoint. It serves Discord
// interactions from a Lambda Function URL.
//
// Build: GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -tags lambda.norpc -o build/bootstrap ./cmd/lambda
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
	_ "time/tzdata" // the Lambda base image has no zoneinfo; Asia/Manila must resolve

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"

	"cloud-control/internal/config"
	"cloud-control/internal/core"
	"cloud-control/internal/interactions"
	"cloud-control/internal/provider"
	"cloud-control/internal/provider/fake"
	"cloud-control/internal/store/memstore"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	h, err := build(log)
	if err != nil {
		log.Error("init failed", "err", err)
		os.Exit(1)
	}
	lambda.Start(func(ctx context.Context, req events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
		return serve(ctx, h, req), nil
	})
}

func logLevel() slog.Level {
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

// build runs once per cold start; warm invocations reuse the handler.
func build(log *slog.Logger) (*interactions.Handler, error) {
	path := os.Getenv("CONFIG_PATH")
	if path == "" {
		path = "config.yaml"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		return nil, err
	}
	sec := config.SecretsFromEnv()
	var missing []string
	for name, v := range map[string]string{
		"DISCORD_PUBLIC_KEY": sec.DiscordPublicKey, "OWNER_DISCORD_USER_ID": sec.OwnerDiscordUserID,
		"DISCORD_GUILD_ID": sec.DiscordGuildID,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing environment variables: %s", strings.Join(missing, ", "))
	}
	cfg.Secrets = sec

	var providers []provider.Provider
	if cfg.Dev.FakeProvider {
		providers = append(providers, fake.Demo())
	}
	if len(providers) == 0 {
		return nil, errors.New("no providers enabled (only the fake provider exists so far)")
	}
	verifier, err := interactions.NewVerifier(sec.DiscordPublicKey, nil)
	if err != nil {
		return nil, err
	}
	// Until DynamoDB (milestone 5) state lives in memory per instance, and
	// alerts are dropped until the REST notifier (milestone 3).
	svc := core.New(core.Options{Config: cfg, Store: memstore.New(), Apps: core.NewResolver(cfg),
		Notifier: core.Discard{}, Logger: log, Providers: providers})
	log.Info("cold start", "apps", cfg.AppNames(), "providers", svc.ProviderNames())
	return &interactions.Handler{Verifier: verifier, OwnerID: sec.OwnerDiscordUserID, GuildID: sec.DiscordGuildID,
		Service: svc, Log: log, Started: time.Now(), Region: os.Getenv("AWS_REGION")}, nil
}

func serve(ctx context.Context, h *interactions.Handler, req events.LambdaFunctionURLRequest) events.LambdaFunctionURLResponse {
	if req.RequestContext.HTTP.Method != "POST" {
		return events.LambdaFunctionURLResponse{StatusCode: 405, Body: "POST only"}
	}
	body := []byte(req.Body)
	if req.IsBase64Encoded {
		b, err := base64.StdEncoding.DecodeString(req.Body)
		if err != nil {
			return events.LambdaFunctionURLResponse{StatusCode: 400, Body: "bad body encoding"}
		}
		body = b
	}
	// Function URLs lowercase header names.
	r := h.Handle(ctx, interactions.Request{
		Signature: req.Headers["x-signature-ed25519"],
		Timestamp: req.Headers["x-signature-timestamp"],
		Body:      body,
	})
	ct := "application/json"
	if r.Status != 200 {
		ct = "text/plain"
	}
	return events.LambdaFunctionURLResponse{StatusCode: r.Status, Headers: map[string]string{"content-type": ct}, Body: string(r.Body)}
}
