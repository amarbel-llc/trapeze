package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/charmbracelet/crush/internal/xmppbridge"
	"github.com/spf13/cobra"
)

var xmppBridgeCmd = &cobra.Command{
	Use:   "xmpp-bridge",
	Short: "Bridge an XMPP MUC room to a clown chat channel",
	Long: `Bridge a single XMPP Multi-User Chat (MUC) room to a single clown
cross-session chat channel, so a human in an XMPP client can converse with the
agent running under a spinclass/clown session.

Inbound groupchat messages are relayed to the agent via 'clown chat send';
agent replies (drained from 'clown chat read') are posted back into the room.
The bridge shells out to the clown binary and runs until interrupted.

Example:

  trapeze xmpp-bridge \
    --jid bridge@krone.example \
    --password "$BRIDGE_PASSWORD" \
    --room session-abc123@conference.krone.example \
    --group abc123`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		cfg := xmppbridge.Config{}
		cfg.JID, _ = cmd.Flags().GetString("jid")
		cfg.Password, _ = cmd.Flags().GetString("password")
		cfg.Server, _ = cmd.Flags().GetString("server")
		cfg.Insecure, _ = cmd.Flags().GetBool("insecure")
		cfg.Room, _ = cmd.Flags().GetString("room")
		cfg.Nick, _ = cmd.Flags().GetString("nick")
		cfg.ClownBin, _ = cmd.Flags().GetString("clown-bin")
		cfg.Group, _ = cmd.Flags().GetString("group")
		cfg.BridgeKey, _ = cmd.Flags().GetString("bridge-key")
		cfg.PollInterval, _ = cmd.Flags().GetDuration("poll-interval")

		if cfg.Password == "" {
			cfg.Password = os.Getenv("TRAPEZE_XMPP_PASSWORD")
		}
		if cfg.JID == "" || cfg.Room == "" || cfg.Group == "" {
			return fmt.Errorf("--jid, --room, and --group are required")
		}

		// Run until SIGINT/SIGTERM.
		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		if err := xmppbridge.Run(ctx, cfg); err != nil && ctx.Err() == nil {
			return err
		}
		return nil
	},
}

func init() {
	f := xmppBridgeCmd.Flags()
	f.String("jid", "", "bridge bot bare JID (e.g. bridge@example.net)")
	f.String("password", "", "bridge bot password (or set TRAPEZE_XMPP_PASSWORD)")
	f.String("server", "", "optional host:port dial override (default: SRV resolution)")
	f.Bool("insecure", false, "skip TLS verification (dev servers only)")
	f.String("room", "", "bare MUC room JID (e.g. session-x@conference.example.net)")
	f.String("nick", "bridge", "in-room nickname")
	f.String("clown-bin", "clown", "clown binary to shell out to")
	f.String("group", "", "clown chat target / CLOWN_GROUP_ID (the spinclass session id)")
	f.String("bridge-key", "", "CLOWN_SESSION_ID for the bridge (default: xmpp-bridge:<group>)")
	f.Duration("poll-interval", 2*time.Second, "how often to drain clown chat read")
}
