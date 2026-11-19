package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nxckdx/longhorntx/internal/kube"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// Shell completion with dynamic names. Names come from a small on-disk cache
// (per context and namespace, 10 minutes), because listing all volumes on
// every TAB would cost hundreds of KB on large clusters.

const completionTTL = 10 * time.Minute

type nameCache struct {
	Updated map[string]time.Time `json:"updated"`
	Names   map[string][]string  `json:"names"`
}

func (g *globals) cachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	ctxName := g.kube.Context
	if ctxName == "" {
		if _, cur, err := (kube.Options{Kubeconfig: g.kube.Kubeconfig}).Contexts(); err == nil {
			ctxName = cur
		}
	}
	safe := regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(ctxName+"_"+g.namespace, "_")
	return filepath.Join(dir, "lhtx", "complete-"+safe+".json")
}

// cachedNames returns names of the given kind, loading them with fetch if the
// cache is missing or older than completionTTL.
func (g *globals) cachedNames(kind string, fetch func(context.Context, *longhorn.Client) ([]string, error)) []string {
	path := g.cachePath()
	var nc nameCache
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(b, &nc)
		}
	}
	if nc.Names == nil {
		nc.Names, nc.Updated = map[string][]string{}, map[string]time.Time{}
	}
	if t, ok := nc.Updated[kind]; ok && time.Since(t) < completionTTL {
		return nc.Names[kind]
	}
	c, err := g.client()
	if err != nil {
		return nc.Names[kind] // stale is better than nothing
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	names, err := fetch(ctx, c)
	if err != nil {
		return nc.Names[kind]
	}
	sort.Strings(names)
	nc.Names[kind], nc.Updated[kind] = names, time.Now()
	if path != "" {
		_ = os.MkdirAll(filepath.Dir(path), 0o700)
		if b, err := json.Marshal(nc); err == nil {
			_ = os.WriteFile(path, b, 0o600)
		}
	}
	return names
}

func filterPrefix(names []string, prefix string) []string {
	var out []string
	for _, n := range names {
		if strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	return out
}

// completeVolumes offers namespace/pvc for bound volumes and the volume name
// for unbound ones.
func (g *globals) completeVolumes(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := g.cachedNames("volumes", func(ctx context.Context, c *longhorn.Client) ([]string, error) {
		vols, err := c.Volumes(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(vols))
		for _, v := range vols {
			if p := v.PVC(); p != "" {
				out = append(out, p)
			} else {
				out = append(out, v.Name)
			}
		}
		return out, nil
	})
	return filterPrefix(names, toComplete), cobra.ShellCompDirectiveNoFileComp
}

// completeNodeDisk offers NODE and NODE:PATH.
func (g *globals) completeNodeDisk(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := g.cachedNames("nodedisks", func(ctx context.Context, c *longhorn.Client) ([]string, error) {
		nodes, err := c.Nodes(ctx)
		if err != nil {
			return nil, err
		}
		var out []string
		for _, n := range nodes {
			out = append(out, n.Name)
			for _, d := range n.Spec.Disks {
				out = append(out, n.Name+":"+d.Path)
			}
		}
		return out, nil
	})
	// zsh/bash treat ':' specially; NoSpace lets the user continue after NODE
	return filterPrefix(names, toComplete), cobra.ShellCompDirectiveNoFileComp
}

func (g *globals) completeReplicas(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := g.cachedNames("replicas", func(ctx context.Context, c *longhorn.Client) ([]string, error) {
		reps, err := c.Replicas(ctx, "")
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(reps))
		for _, r := range reps {
			out = append(out, r.Name)
		}
		return out, nil
	})
	return filterPrefix(names, toComplete), cobra.ShellCompDirectiveNoFileComp
}

func (g *globals) completeSettings(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := g.cachedNames("settings", func(ctx context.Context, c *longhorn.Client) ([]string, error) {
		sets, err := c.Settings(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(sets))
		for _, s := range sets {
			out = append(out, s.Name)
		}
		return out, nil
	})
	return filterPrefix(names, toComplete), cobra.ShellCompDirectiveNoFileComp
}

// completeSnapshots completes snapshot names (for delete snapshot). Loaded
// fresh each time from a small label-free list would be too big; we cache too.
func (g *globals) completeSnapshots(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := g.cachedNames("snapshots", func(ctx context.Context, c *longhorn.Client) ([]string, error) {
		snaps, err := c.Snapshots(ctx, "")
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(snaps))
		for _, s := range snaps {
			out = append(out, s.Name)
		}
		return out, nil
	})
	return filterPrefix(names, toComplete), cobra.ShellCompDirectiveNoFileComp
}

// registerCompletions wires completion functions into the command tree.
func registerCompletions(root *cobra.Command, g *globals) {
	_ = root.RegisterFlagCompletionFunc("context", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		names, _, _ := (kube.Options{Kubeconfig: g.kube.Kubeconfig}).Contexts()
		return names, cobra.ShellCompDirectiveNoFileComp
	})
	_ = root.RegisterFlagCompletionFunc("output", cobra.FixedCompletions([]string{"table", "wide", "json"}, cobra.ShellCompDirectiveNoFileComp))
	_ = root.RegisterFlagCompletionFunc("refresh", cobra.FixedCompletions([]string{"auto", "watch", "30s", "1m", "2m"}, cobra.ShellCompDirectiveNoFileComp))
	_ = root.RegisterFlagCompletionFunc("dry-run", cobra.FixedCompletions([]string{"client", "server"}, cobra.ShellCompDirectiveNoFileComp))

	for _, c := range root.Commands() {
		if c.Name() == "doctor" {
			_ = c.RegisterFlagCompletionFunc("min", cobra.FixedCompletions([]string{"info", "warning", "critical"}, cobra.ShellCompDirectiveNoFileComp))
		}
	}

	byPath := map[string]cobra.CompletionFunc{
		"replicas":        g.completeVolumes,
		"snapshots":       g.completeVolumes,
		"backups":         g.completeVolumes,
		"events":          g.completeVolumes,
		"jobs":            g.completeVolumes,
		"disk":            g.completeNodeDisk,
		"cordon":          g.completeNodeDisk,
		"uncordon":        g.completeNodeDisk,
		"evict":           g.completeNodeDisk,
		"create snapshot": g.completeVolumes,
		"create backup":   g.completeVolumes,
		"delete snapshot": g.completeSnapshots,
		"delete replica":  g.completeReplicas,
		"set replicas":    g.completeVolumes,
		"set setting":     g.completeSettings,
	}
	var walk func(c *cobra.Command, prefix string)
	walk = func(c *cobra.Command, prefix string) {
		for _, sub := range c.Commands() {
			path := strings.TrimSpace(prefix + " " + sub.Name())
			if f, ok := byPath[path]; ok {
				sub.ValidArgsFunction = f
			}
			walk(sub, path)
		}
	}
	walk(root, "")
}
