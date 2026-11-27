package longhorn

import (
	"testing"
	"time"
)

func TestForecast(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	const gib = int64(1) << 30
	var s []CapacitySample
	// grows 10 GiB a day; 100 of 200 GiB used now → full in 10 days
	for d := 10; d >= 0; d-- {
		s = append(s, CapacitySample{At: now.Add(-time.Duration(d) * 24 * time.Hour), Disks: map[string]DiskUse{
			"n1:/data":   {Used: 100*gib - int64(d)*10*gib, Usable: 200 * gib},
			"n2:/data":   {Used: 50 * gib, Usable: 200 * gib},
			"n3:/shrink": {Used: 50*gib + int64(d)*gib, Usable: 200 * gib},
		}})
	}
	s = append(s, CapacitySample{At: now.Add(-time.Hour), Disks: map[string]DiskUse{"n4:/new": {Used: gib, Usable: 10 * gib}}})
	got := map[string]DiskForecast{}
	for _, f := range Forecast(s, now) {
		got[f.Disk] = f
	}
	if f := got["n1:/data"]; f.FullIn != 10*24*time.Hour || f.PerDay != 10*gib {
		t.Errorf("growing disk: %+v", f)
	}
	if f := got["n2:/data"]; f.FullIn != 0 || f.PerDay != 0 {
		t.Errorf("flat disk: %+v", f)
	}
	if f := got["n3:/shrink"]; f.FullIn != 0 || f.PerDay >= 0 {
		t.Errorf("shrinking disk: %+v", f)
	}
	if f := got["n4:/new"]; !f.TooLittle {
		t.Errorf("one sample is too little: %+v", f)
	}
}

func TestSampleCapacityRoundTrip(t *testing.T) {
	n := &Node{}
	n.Name = "n1"
	n.Spec.Disks = map[string]DiskSpec{"d1": {Path: "/var/lib/longhorn", StorageReserved: Quantity(10)}}
	n.Status.DiskStatus = map[string]DiskStatus{"d1": {DiskPath: "/var/lib/longhorn", StorageMaximum: Quantity(100), StorageAvailable: Quantity(60)}}
	s := SampleCapacity([]*Node{n}, time.Now())
	if d := s.Disks["n1:/var/lib/longhorn"]; d.Used != 40 || d.Usable != 90 {
		t.Fatalf("sample: %+v", s)
	}
	obj, err := CapacityObject("lh", []CapacitySample{s, s}, 1)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeCapacity(obj)
	if err != nil || len(back) != 1 || back[0].Disks["n1:/var/lib/longhorn"].Used != 40 {
		t.Fatalf("round trip: %+v %v", back, err)
	}
}

func TestRestoreProbeVolumeEncrypted(t *testing.T) {
	b := &Backup{}
	b.Name, b.Status.State, b.Status.URL, b.Status.VolumeSize = "b1", "Completed", "nfs://x?backup=b1", Quantity(1<<30)
	obj, err := RestoreProbeVolume(b, "lhtx-probe-x")
	if err != nil || obj["spec"].(map[string]any)["encrypted"] != nil {
		t.Fatalf("plain: %v %v", obj["spec"], err)
	}
	// an encrypted backup needs an encrypted test volume (room for the LUKS
	// header; Longhorn sets the size of a restore itself)
	b.Status.Labels = map[string]string{"volume-encrypted": "enabled"}
	obj, _ = RestoreProbeVolume(b, "lhtx-probe-x")
	if obj["spec"].(map[string]any)["encrypted"] != true || obj["spec"].(map[string]any)["size"] != "1073741824" {
		t.Errorf("encrypted: %v", obj["spec"])
	}
}
