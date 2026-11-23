package longhorn

import (
	"context"
	"time"
)

// LoadDoctorInput reads what Diagnose needs; engines and snapshots only for
// rebuilding volumes.
func LoadDoctorInput(ctx context.Context, c *Client) (DoctorInput, error) {
	var in DoctorInput
	var err error
	if in.Volumes, err = c.Volumes(ctx); err != nil {
		return in, err
	}
	if in.Replicas, err = c.Replicas(ctx, ""); err != nil {
		return in, err
	}
	if in.Nodes, err = c.Nodes(ctx); err != nil {
		return in, err
	}
	for _, v := range RebuildingVolumes(in.Replicas) {
		engs, err := c.Engines(ctx, v)
		if err != nil {
			return in, err
		}
		in.Engines = append(in.Engines, engs...)
		snaps, err := c.Snapshots(ctx, v)
		if err != nil {
			return in, err
		}
		in.Snapshots = append(in.Snapshots, snaps...)
	}
	// small collections; failures here should not hide the other checks
	in.Targets, _ = c.BackupTargets(ctx)
	in.Jobs, _ = c.RecurringJobs(ctx)
	in.Orphans, _, _ = c.ListCount(ctx, "orphans")
	in.Now = time.Now()
	return in, nil
}
