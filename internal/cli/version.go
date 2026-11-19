package cli

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/nxckdx/longhorntx/internal/update"
)

func versionCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show the version and whether a newer release exists",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Printf("lhtx %s (%s/%s, %s)\n", Version, runtime.GOOS, runtime.GOARCH, runtime.Version())
			switch {
			case Version == "dev":
				fmt.Println("development build: no update check")
			case update.InNixStore():
				fmt.Println("installed through Nix: update with `nix profile upgrade` or your flake inputs")
			default:
				r, err := update.Latest(cmd.Context(), Version)
				if err != nil {
					return fmt.Errorf("update check: %w", err)
				}
				if update.Newer(r.Version(), Version) {
					fmt.Printf("update available: %s (%s) – run `lhtx update`\n", r.Version(), r.URL)
				} else {
					fmt.Println("up to date")
				}
			}
			return nil
		},
	}
}

func updateCmd(g *globals) *cobra.Command {
	var checkOnly bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update lhtx from the GitHub releases (not for Nix installs)",
		Long: `Download the latest release for this platform from GitHub, verify its
SHA256 checksum and replace the running binary. Nix installs are updated
through Nix instead.

lhtx checks for new releases at most once a day (TUI header, ~2 KB); set
LHTX_NO_UPDATE_CHECK=1 to turn that off.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if update.InNixStore() {
				return fmt.Errorf("lhtx is installed through Nix: update with `nix profile upgrade` or your flake inputs")
			}
			r, err := update.Latest(cmd.Context(), Version)
			if err != nil {
				return err
			}
			if Version != "dev" && !update.Newer(r.Version(), Version) {
				fmt.Printf("lhtx %s is up to date\n", Version)
				return nil
			}
			fmt.Printf("lhtx %s → %s (%s)\n", Version, r.Version(), r.URL)
			if checkOnly {
				return nil
			}
			if !g.yes {
				if !term.IsTerminal(int(os.Stdin.Fd())) {
					return fmt.Errorf("no terminal for confirmation: add --yes")
				}
				fmt.Print("Install? [y/N] ")
				s, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if s = strings.ToLower(strings.TrimSpace(s)); s != "y" && s != "yes" {
					return fmt.Errorf("aborted")
				}
			}
			if err := update.Install(cmd.Context(), r, Version, func(s string) { fmt.Println("… " + s) }); err != nil {
				return err
			}
			fmt.Printf("✔ updated to %s\n", r.Version())
			return nil
		},
	}
	cmd.Flags().BoolVar(&checkOnly, "check", false, "only report whether an update exists")
	return cmd
}
