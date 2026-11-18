package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hasankaradmir/karga-sdk/api"
)

func newGatewayCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gateway",
		Short: "Configure the host gateway: where it listens and on which listeners",
		Long: "The gateway is the host's single entry point. Its administrator decides where it listens\n" +
			"(set listen, default 127.0.0.1) and on which ports (listeners); packages only attach their\n" +
			"Services to listeners by name and can open nothing themselves.",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show the gateway's health and the listeners and routes it serves",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := a.backend()
			health, err := s.GatewayHealth(cmd.Context())
			if err != nil {
				return err
			}
			routes, err := s.GatewayRoutes(cmd.Context())
			if err != nil {
				return err
			}
			if a.structured() {
				if routes == nil {
					routes = []api.Exposed{}
				}
				return a.write(map[string]any{"gateway": health, "routes": routes})
			}
			state := health.Container
			switch {
			case health.Container == "missing":
				state = "not running (nothing is exposed)"
			case health.Ready:
				state += ", ready (Envoy " + health.Version + ")"
			case health.Error != "":
				state += ", admin API unreachable: " + health.Error
			default:
				state += ", not ready"
			}
			access := "off"
			if health.AccessLog {
				access = "on (karga logs --gateway)"
			}
			_, _ = fmt.Fprintf(a.stdout, "Gateway:    %s\n", state)
			if health.Image != "" {
				_, _ = fmt.Fprintf(a.stdout, "Image:      %s\n", health.Image)
			}
			_, _ = fmt.Fprintf(a.stdout, "Listen:     %s\n", strings.Join(health.Listen, ", "))
			_, _ = fmt.Fprintf(a.stdout, "Access log: %s\n\n", access)
			var rows [][]string
			for _, r := range routes {
				rows = append(rows, []string{r.Listener, r.Route, r.Release, r.Service, r.Upstream})
			}
			return a.table("LISTENER\tROUTE\tRELEASE\tSERVICE\tUPSTREAM", rows)
		},
	})
	set := &cobra.Command{Use: "set", Short: "Change a gateway setting"}
	set.AddCommand(&cobra.Command{
		Use:       "access-log on|off",
		Short:     "Log every request and connection the gateway handles (off by default)",
		Args:      cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		ValidArgs: []string{"on", "off"},
		RunE: func(cmd *cobra.Command, args []string) error {
			s := a.backend()
			if err := s.SetGatewayAccessLog(cmd.Context(), api.GatewayAccessLogRequest{Enabled: args[0] == "on"}); err != nil {
				return err
			}
			return a.report(map[string]any{"accessLog": args[0] == "on"}, "gateway access log %s\n", args[0])
		},
	})
	set.AddCommand(&cobra.Command{
		Use:   "listen <ip>[,<ip>...]",
		Short: "Set the host addresses every listener binds (default 127.0.0.1; 0.0.0.0 is every interface)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var addrs []string
			for _, ip := range strings.Split(args[0], ",") {
				addrs = append(addrs, strings.TrimSpace(ip))
			}
			if exposesHost(addrs) {
				if err := a.confirm(cmd, fmt.Sprintf("Every exposed service becomes reachable on %s, from other machines too", strings.Join(addrs, ", "))); err != nil {
					return err
				}
			}
			s := a.backend()
			if err := s.SetGatewayListen(cmd.Context(), api.GatewayListenRequest{Addresses: addrs}); err != nil {
				return err
			}
			return a.report(map[string]any{"listen": addrs}, "gateway listens on %s\n", strings.Join(addrs, ", "))
		},
	})
	cmd.AddCommand(set, newListenerCmd(a))
	return cmd
}

func newListenerCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "listener", Short: "Define the ports services can be exposed on"}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List the listeners and the services using them",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := a.backend()
			settings, users, err := s.GatewaySettings(cmd.Context())
			if err != nil {
				return err
			}
			if a.structured() {
				return a.write(map[string]any{"listen": settings.Listen, "listeners": settings.Listeners, "users": users})
			}
			var rows [][]string
			for _, l := range settings.Listeners {
				rows = append(rows, []string{l.Name, strconv.Itoa(l.Port), l.Protocol, strings.Join(users[l.Name], ", ")})
			}
			return a.table("NAME\tPORT\tPROTOCOL\tSERVICES", rows)
		},
	})
	var l api.Listener
	add := &cobra.Command{
		Use:   "add <name> --port <port> --protocol tcp|http|https",
		Short: "Define a listener (a tcp listener belongs to one service; http and https are shared by host)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l.Name = args[0]
			s := a.backend()
			if err := s.AddGatewayListener(cmd.Context(), l); err != nil {
				return err
			}
			return a.report(l, "listener %s added (%s on port %d)\n", l.Name, l.Protocol, l.Port)
		},
	}
	add.Flags().IntVar(&l.Port, "port", 0, "host port")
	add.Flags().StringVar(&l.Protocol, "protocol", "", "tcp, http or https")
	_ = add.MarkFlagRequired("port")
	_ = add.MarkFlagRequired("protocol")
	cmd.AddCommand(add, &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a listener no service uses",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.confirm(cmd, fmt.Sprintf("This removes listener %s from the gateway", args[0])); err != nil {
				return err
			}
			s := a.backend()
			if err := s.RemoveGatewayListener(cmd.Context(), api.GatewayListenerRequest{Name: args[0]}); err != nil {
				return err
			}
			return a.report(map[string]any{"listener": args[0], "removed": true}, "listener %s removed\n", args[0])
		},
	})
	return cmd
}
