package longhorn

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDiagnose(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	var in DoctorInput
	in.Now = now
	in.Volumes = []*Volume{must[Volume](t, `{"metadata":{"name":"vol","labels":{"recurring-job-group.longhorn.io/default":"enabled"}},
		"spec":{"size":"10737418240","numberOfReplicas":2},
		"status":{"state":"attached","robustness":"degraded","lastBackupAt":"2026-11-16T01:00:00Z",
		"kubernetesStatus":{"namespace":"app","pvcName":"data"}}}`)}
	in.Replicas = []*Replica{
		must[Replica](t, `{"metadata":{"name":"vol-r-aaaa"},"spec":{"volumeName":"vol","nodeID":"n1","diskID":"u1","healthyAt":"2026-01-01T00:00:00Z"},"status":{"currentState":"running"}}`),
		must[Replica](t, `{"metadata":{"name":"vol-r-bbbb"},"spec":{"volumeName":"vol","nodeID":"n1","diskID":"u1"},"status":{"currentState":"running","storageIP":"10.0.0.1","port":2}}`),
	}
	in.Nodes = []*Node{must[Node](t, `{"metadata":{"name":"n1"},"spec":{"allowScheduling":true,"disks":{"d1":{"path":"/data","allowScheduling":true}}},
		"status":{"conditions":[{"type":"Ready","status":"True"},{"type":"Schedulable","status":"True"}],
		"diskStatus":{"d1":{"diskUUID":"u1","diskPath":"/data","storageMaximum":1000,"storageAvailable":80,
		"conditions":[{"type":"Ready","status":"True"},{"type":"Schedulable","status":"False","message":"disk pressure"}],
		"healthData":{"sda":{"healthStatus":"PASSED","modelName":"SSD","attributes":[{"name":"Percent_Lifetime_Remain","rawValue":85,"value":15}]}}}}}}`)}
	in.Engines = []*Engine{must[Engine](t, `{"metadata":{"name":"vol-e-0"},"spec":{"volumeName":"vol","active":true},
		"status":{"replicaModeMap":{"vol-r-aaaa":"RW","vol-r-bbbb":"WO"},
		"replicaTransitionTimeMap":{"vol-r-bbbb":"2026-11-19T10:30:00Z"},
		"currentReplicaAddressMap":{"vol-r-bbbb":"10.0.0.1:2"},
		"rebuildStatus":{"tcp://10.0.0.1:2":{"isRebuilding":true,"progress":99}}}}`)}
	in.Snapshots = []*VolumeSnapshot{must[VolumeSnapshot](t, `{"metadata":{"name":"s1"},"spec":{"volume":"vol"},"status":{"markRemoved":true}}`)}
	in.Targets = []*BackupTarget{must[BackupTarget](t, `{"metadata":{"name":"default"},"spec":{"backupTargetURL":"nfs://x"},"status":{"available":false}}`)}
	in.Jobs = []*RecurringJob{must[RecurringJob](t, `{"metadata":{"name":"daily-backup"},"spec":{"task":"backup","cron":"0 1 * * *","groups":["default"]}}`)}

	fs := Diagnose(in)
	got := map[string]Finding{}
	for _, f := range fs {
		got[f.Check] = f
	}
	expect := map[string]Severity{
		"volume": SevWarning, "redundancy": SevWarning, "placement": SevWarning,
		"rebuild-blocked": SevWarning, "disk": SevCritical, "smart": SevWarning,
		"backup-target": SevCritical, "backup-age": SevWarning,
	}
	for check, sev := range expect {
		f, ok := got[check]
		if !ok {
			t.Errorf("missing finding %q", check)
			continue
		}
		if f.Severity != sev && !(check == "disk") {
			t.Errorf("%s: severity %v, want %v (%s)", check, f.Severity, sev, f.Message)
		}
	}
	if b := got["rebuild-blocked"]; !strings.Contains(b.Message, "1h30m") || !strings.Contains(b.Message, "99%") {
		t.Errorf("blocked message: %s", b.Message)
	}
	if fs[0].Severity != SevCritical || Worst(fs) != SevCritical {
		t.Error("findings must be sorted by severity")
	}
	if testing.Verbose() {
		b, _ := json.MarshalIndent(fs, "", "  ")
		t.Log(string(b))
	}
}

func TestCronPeriod(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for spec, want := range map[string]time.Duration{"0 1 * * *": 24 * time.Hour, "*/30 * * * *": 30 * time.Minute, "@weekly": 7 * 24 * time.Hour} {
		if got, ok := cronPeriod(spec, now); !ok || got != want {
			t.Errorf("%s: %v", spec, got)
		}
	}
}
