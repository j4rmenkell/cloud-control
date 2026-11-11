// Command register registers cloud-control's slash commands in your Discord
// server. Run it locally whenever the command list or apps change:
//
//	go run ./cmd/register            # uses .env and config.local.yaml/config.yaml
//	go run ./cmd/register -dry-run   # print the payload only
//
// It overwrites the server's commands for this application, so commands that
// are no longer defined are removed.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/joho/godotenv"

	"cloud-control/internal/config"
	"cloud-control/internal/interactions"
)

func main() {
	envPath := flag.String("env", ".env", "path to .env")
	cfgPath := flag.String("config", "", "config file (default: config.local.yaml if present, else config.yaml)")
	dry := flag.Bool("dry-run", false, "print the payload without calling Discord")
	flag.Parse()
	if err := run(*envPath, *cfgPath, *dry); err != nil {
		fmt.Fprintln(os.Stderr, "register:", err)
		os.Exit(1)
	}
}

func run(envPath, cfgPath string, dry bool) error {
	if err := godotenv.Load(envPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if cfgPath == "" {
		cfgPath = config.DefaultPath(".")
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		return err
	}
	payload, err := json.MarshalIndent(interactions.Commands(cfg.AppNames()), "", "  ")
	if err != nil {
		return err
	}
	if dry {
		fmt.Println(string(payload))
		return nil
	}
	sec := config.SecretsFromEnv()
	if sec.DiscordBotToken == "" || sec.DiscordApplicationID == "" || sec.DiscordGuildID == "" {
		return errors.New("DISCORD_BOT_TOKEN, DISCORD_APPLICATION_ID and DISCORD_GUILD_ID must be set")
	}
	url := fmt.Sprintf("https://discord.com/api/v10/applications/%s/guilds/%s/commands", sec.DiscordApplicationID, sec.DiscordGuildID)
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+sec.DiscordBotToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "DiscordBot (https://github.com/j4rmenkell/cloud-control, 0.2)")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("discord returned %s: %s", resp.Status, body)
	}
	var got []struct{ Name string }
	_ = json.Unmarshal(body, &got)
	fmt.Printf("registered %d commands from %s:", len(got), cfgPath)
	for _, c := range got {
		fmt.Printf(" /%s", c.Name)
	}
	fmt.Println()
	return nil
}
