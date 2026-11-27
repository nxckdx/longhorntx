package cli

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

func capacityCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "capacity",
		Short: "When will the Longhorn disks be full? Trend of the last 14 days – kept by the lhtx agent",
		Long: `Longhorn shows how full a disk is now, not how fast it fills. The lhtx agent
samples every disk every few hours (--capacity-interval) and keeps ~60 days;
this fits a line through the last 14 days and says when each disk reaches
its usable size (maximum minus reserved). lhtx doctor warns below 14 days.
Needs the agent and at least 2 days of samples.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := g.checkOutput(); err != nil {
				return err
			}
			c, err := g.client()
			if err != nil {
				return err
			}
			s, err := c.Capacity(cmd.Context())
			if errors.Is(err, longhorn.ErrNoCapacity) {
				fmt.Println(err)
				return nil
			}
			if err != nil {
				return err
			}
			fs := longhorn.Forecast(s, time.Now())
			if g.output == "json" {
				return writeJSON(os.Stdout, fs)
			}
			w := newTab(os.Stdout)
			fmt.Fprintln(w, "DISK\tUSED\tUSABLE\tUSED%\tPER DAY\tFULL IN")
			for _, f := range fs {
				pct := "-"
				if f.Usable > 0 {
					pct = fmt.Sprintf("%d%%", f.Used*100/f.Usable)
				}
				per, full := "-", "-"
				switch {
				case f.TooLittle:
					full = "(" + f.Span.String() + " of samples – needs 2 days)"
				case f.FullIn > 0:
					per = "+" + format.Bytes(f.PerDay)
					full = fmt.Sprintf("~%d days", int(f.FullIn.Hours()/24))
				case f.PerDay < 0:
					per = "-" + format.Bytes(-f.PerDay)
					full = "not filling up"
				default:
					per = "+" + format.Bytes(f.PerDay)
					full = "not filling up"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", f.Disk, format.Bytes(f.Used), format.Bytes(f.Usable), pct, per, full)
			}
			fmt.Fprintf(w, "\n%d samples, the oldest from %s\n", len(s), s[0].At.Local().Format("2006-01-02 15:04"))
			return w.Flush()
		},
	}
}
