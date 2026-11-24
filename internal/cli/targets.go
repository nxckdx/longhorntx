package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

func targetWriteCmds(g *globals) []*cobra.Command {
	var url, secret, poll string
	flags := func(cmd *cobra.Command) {
		cmd.Flags().StringVar(&url, "url", "", longhorn.TargetURLHelp)
		cmd.Flags().StringVar(&secret, "secret", "", "credential secret in the Longhorn namespace (\"\" for none)")
		cmd.Flags().StringVar(&poll, "poll", "", "how often Longhorn reads the store, e.g. 5m (0 = off)")
		_ = cmd.RegisterFlagCompletionFunc("secret", g.completeSecrets)
	}
	apply := func(cmd *cobra.Command, s *longhorn.TargetSpec) {
		if cmd.Flags().Changed("url") {
			s.URL = url
		}
		if cmd.Flags().Changed("secret") {
			s.Secret = secret
		}
		if cmd.Flags().Changed("poll") {
			s.PollInterval = poll
		}
	}
	load := func(cmd *cobra.Command) (*longhorn.Client, []*longhorn.BackupTarget, []string, error) {
		c, err := g.client()
		if err != nil {
			return nil, nil, nil, err
		}
		ts, err := c.BackupTargets(cmd.Context())
		if err != nil {
			return nil, nil, nil, err
		}
		secrets, _ := c.SecretNames(cmd.Context()) // nil if not allowed: not checked then
		return c, ts, secrets, nil
	}
	find := func(ts []*longhorn.BackupTarget, name string) (*longhorn.BackupTarget, error) {
		for _, t := range ts {
			if t.Name == name {
				return t, nil
			}
		}
		return nil, fmt.Errorf("backup target %q not found", name)
	}

	create := &cobra.Command{
		Use:   "create NAME --url URL [--secret S] [--poll 5m]",
		Short: "Create a backup target (needs --rw; Longhorn ≥ 1.8)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, ts, secrets, err := load(cmd)
			if err != nil {
				return err
			}
			s := longhorn.TargetSpec{Name: args[0]}
			apply(cmd, &s)
			a, err := longhorn.CreateTarget(s, ts, secrets)
			if err != nil {
				return err
			}
			return g.runAction(cmd.Context(), c, a)
		},
	}
	flags(create)
	_ = create.MarkFlagRequired("url")

	edit := &cobra.Command{
		Use:               "edit NAME [--url URL] [--secret S] [--poll 5m]",
		Short:             "Change a backup target (needs --rw); only the given flags change",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: g.completeTargets,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, ts, secrets, err := load(cmd)
			if err != nil {
				return err
			}
			t, err := find(ts, args[0])
			if err != nil {
				return err
			}
			s := longhorn.TargetSpecOf(t)
			apply(cmd, &s)
			a, err := longhorn.UpdateTarget(t, s, ts, secrets)
			if err != nil {
				return err
			}
			return g.runAction(cmd.Context(), c, a)
		},
	}
	flags(edit)

	del := &cobra.Command{
		Use:               "delete NAME",
		Short:             "Delete a backup target (needs --rw; not the default one)",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: g.completeTargets,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, ts, _, err := load(cmd)
			if err != nil {
				return err
			}
			t, err := find(ts, args[0])
			if err != nil {
				return err
			}
			vols, err := c.Volumes(cmd.Context())
			if err != nil {
				return err
			}
			a, err := longhorn.DeleteTarget(t, vols)
			if err != nil {
				return err
			}
			return g.runAction(cmd.Context(), c, a)
		},
	}

	sync := &cobra.Command{
		Use:               "sync NAME",
		Short:             "Make Longhorn re-read the backup store now (needs --rw)",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: g.completeTargets,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, ts, _, err := load(cmd)
			if err != nil {
				return err
			}
			t, err := find(ts, args[0])
			if err != nil {
				return err
			}
			return g.runAction(cmd.Context(), c, longhorn.SyncTarget(t, time.Now()))
		},
	}
	return []*cobra.Command{create, edit, del, sync}
}

func (g *globals) completeTargets(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return g.cachedNames("targets", func(ctx context.Context, c *longhorn.Client) ([]string, error) {
		ts, err := c.BackupTargets(ctx)
		var out []string
		for _, t := range ts {
			out = append(out, t.Name)
		}
		return out, err
	}), cobra.ShellCompDirectiveNoFileComp
}

func (g *globals) completeSecrets(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	c, err := g.client()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names, _ := c.SecretNames(cmd.Context())
	return names, cobra.ShellCompDirectiveNoFileComp
}
