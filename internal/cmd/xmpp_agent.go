package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/charmbracelet/crush/internal/xmppagent"
	"github.com/spf13/cobra"
)

var xmppAgentCmd = &cobra.Command{
	Use:   "xmpp-agent",
	Short: "Run a headless, pure-conversational XMPP agent backed by OpenRouter",
	Long: `Run trapeze headless as a 1:1 XMPP chat agent: it logs in to XMPP and,
for each direct message it receives, runs an OpenRouter chat completion (with
per-peer conversation history) and replies. No TUI, no tools — pure chat.

This is the trapeze half of clown's headless 'trapeze' provider; clown launches
it with the OpenRouter env populated. It can also be run directly:

  OPENROUTER_API_KEY=sk-... trapeze xmpp-agent \
    --jid agent@krone --password "$PW" --server 127.0.0.1:5222 --insecure \
    --model openai/gpt-4o-mini

DM the agent's JID (agent@krone) from any XMPP client to chat with it.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		cfg := xmppagent.Config{}
		cfg.JID, _ = cmd.Flags().GetString("jid")
		cfg.Password, _ = cmd.Flags().GetString("password")
		cfg.Server, _ = cmd.Flags().GetString("server")
		cfg.Insecure, _ = cmd.Flags().GetBool("insecure")
		cfg.BaseURL, _ = cmd.Flags().GetString("base-url")
		cfg.APIKey, _ = cmd.Flags().GetString("api-key")
		cfg.Model, _ = cmd.Flags().GetString("model")
		cfg.SystemPrompt, _ = cmd.Flags().GetString("system-prompt")
		cfg.MaxHistory, _ = cmd.Flags().GetInt("max-history")

		// Env fallbacks (clown's trapeze provider populates these).
		if cfg.Password == "" {
			cfg.Password = os.Getenv("TRAPEZE_XMPP_PASSWORD")
		}
		if cfg.APIKey == "" {
			cfg.APIKey = os.Getenv("OPENROUTER_API_KEY")
		}
		if cfg.BaseURL == "" {
			cfg.BaseURL = os.Getenv("OPENROUTER_BASE_URL")
		}
		if cfg.Model == "" {
			cfg.Model = os.Getenv("OPENROUTER_MODEL")
		}

		if cfg.JID == "" {
			return fmt.Errorf("--jid is required")
		}
		if cfg.APIKey == "" {
			return fmt.Errorf("OpenRouter API key required (--api-key or OPENROUTER_API_KEY)")
		}
		if cfg.Model == "" {
			return fmt.Errorf("OpenRouter model required (--model or OPENROUTER_MODEL)")
		}

		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		if err := xmppagent.Run(ctx, cfg); err != nil && ctx.Err() == nil {
			return err
		}
		return nil
	},
}

func init() {
	f := xmppAgentCmd.Flags()
	f.String("jid", "", "agent bare JID (e.g. agent@krone)")
	f.String("password", "", "agent password (or set TRAPEZE_XMPP_PASSWORD)")
	f.String("server", "", "optional host:port dial override (default: SRV resolution)")
	f.Bool("insecure", false, "skip TLS verification (dev servers only)")
	f.String("base-url", "", "OpenRouter base URL (or OPENROUTER_BASE_URL; default https://openrouter.ai/api/v1)")
	f.String("api-key", "", "OpenRouter API key (or OPENROUTER_API_KEY)")
	f.String("model", "", "OpenRouter model id (or OPENROUTER_MODEL), e.g. openai/gpt-4o-mini")
	f.String("system-prompt", "", "system prompt seeded into every conversation")
	f.Int("max-history", 40, "max per-peer turns retained")
}
