package longhorn

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Managing BackupTarget CRs (Longhorn ≥ 1.8: several targets; "default" is
// the one volumes use unless spec.backupTargetName says otherwise).

// TargetSpec is the editable part of a backup target.
type TargetSpec struct {
	Name         string
	URL          string
	Secret       string // credential secret in the Longhorn namespace
	PollInterval string // e.g. 5m0s; "0" disables polling the store
}

func TargetSpecOf(t *BackupTarget) TargetSpec {
	return TargetSpec{Name: t.Name, URL: t.Spec.BackupTargetURL, Secret: t.Spec.CredentialSecret, PollInterval: t.Spec.PollInterval}
}

var targetSchemes = []string{"s3", "nfs", "cifs", "azblob"}

// TargetURLHelp shows the formats longhorn-manager expects.
const TargetURLHelp = "s3://bucket@region/path · nfs://server:/export/path · cifs://server/share/path · azblob://container@endpoint/path"

// Validate checks the URL format and the poll interval. secrets are the
// secret names in the Longhorn namespace (nil = unknown, not checked).
func (s TargetSpec) Validate(others []*BackupTarget, secrets []string) error {
	if !nameRE.MatchString(s.Name) {
		return fmt.Errorf("name %q: lowercase letters, digits and '-'", s.Name)
	}
	if s.URL == "" {
		return nil // disables the target (how the default target is turned off)
	}
	scheme, rest, ok := strings.Cut(s.URL, "://")
	if !ok || !slices.Contains(targetSchemes, scheme) || rest == "" {
		return fmt.Errorf("URL %q: %s", s.URL, TargetURLHelp)
	}
	switch scheme {
	case "s3", "azblob":
		bucket, _, ok := strings.Cut(rest, "@")
		if !ok || bucket == "" {
			return fmt.Errorf("URL %q: %s://<bucket>@<region or endpoint>/<path>", s.URL, scheme)
		}
	case "nfs":
		if !strings.Contains(rest, ":/") {
			return fmt.Errorf("URL %q: nfs://<server>:/<export path>", s.URL)
		}
	}
	if s.PollInterval != "" && s.PollInterval != "0" {
		if d, err := time.ParseDuration(s.PollInterval); err != nil || d < 0 {
			return fmt.Errorf("poll interval %q: a duration like 5m, 300s or 0 (off)", s.PollInterval)
		}
	}
	for _, o := range others {
		if o.Name != s.Name && o.Spec.BackupTargetURL == s.URL {
			return fmt.Errorf("backup target %s already uses %s", o.Name, s.URL)
		}
	}
	if s.Secret != "" && secrets != nil && !slices.Contains(secrets, s.Secret) {
		return fmt.Errorf("secret %q does not exist in the Longhorn namespace", s.Secret)
	}
	return nil
}

func (s TargetSpec) warnings() []string {
	var w []string
	if s.URL == "" {
		w = append(w, "empty URL: the target is disabled, backups of volumes using it fail")
	}
	if strings.HasPrefix(s.URL, "s3://") && s.Secret == "" {
		w = append(w, "s3 without a credential secret only works with instance roles (e.g. IRSA on EKS)")
	}
	if strings.HasPrefix(s.URL, "cifs://") && s.Secret == "" {
		w = append(w, "cifs usually needs a credential secret (CIFS_USERNAME, CIFS_PASSWORD)")
	}
	if s.PollInterval == "0" || s.PollInterval == "0s" {
		w = append(w, "poll interval 0: Longhorn no longer syncs backups from the store (new backups of other clusters stay invisible)")
	}
	return w
}

func CreateTarget(s TargetSpec, others []*BackupTarget, secrets []string) (*Action, error) {
	if s.PollInterval == "" {
		s.PollInterval = "5m0s"
	}
	if err := s.Validate(others, secrets); err != nil {
		return nil, err
	}
	for _, o := range others {
		if o.Name == s.Name {
			return nil, fmt.Errorf("backup target %q exists", s.Name)
		}
	}
	return &Action{
		Title: fmt.Sprintf("Create backup target %s (%s)", s.Name, s.URL),
		Verb:  VerbCreate, GVR: GVRBackupTarget, Name: s.Name,
		Object: lhObject("BackupTarget", s.Name, nil, map[string]any{
			"backupTargetURL": s.URL, "credentialSecret": s.Secret, "pollInterval": s.PollInterval,
		}),
		Warnings: append(s.warnings(), "volumes use it when their backupTargetName is "+s.Name+" (new volumes: StorageClass parameter backupTargetName)"),
	}, nil
}

// UpdateTarget patches URL, secret and poll interval.
func UpdateTarget(t *BackupTarget, s TargetSpec, others []*BackupTarget, secrets []string) (*Action, error) {
	s.Name = t.Name
	if err := s.Validate(others, secrets); err != nil {
		return nil, err
	}
	old := TargetSpecOf(t)
	patch := map[string]any{}
	var ch []Change
	if s.URL != old.URL {
		patch["backupTargetURL"], ch = s.URL, append(ch, Change{"spec.backupTargetURL", old.URL, s.URL})
	}
	if s.Secret != old.Secret {
		patch["credentialSecret"], ch = s.Secret, append(ch, Change{"spec.credentialSecret", old.Secret, s.Secret})
	}
	if s.PollInterval != old.PollInterval {
		patch["pollInterval"], ch = s.PollInterval, append(ch, Change{"spec.pollInterval", old.PollInterval, s.PollInterval})
	}
	if len(patch) == 0 {
		return nil, fmt.Errorf("nothing changed")
	}
	a := &Action{
		Title: "Change backup target " + t.Name,
		Verb:  VerbPatch, GVR: GVRBackupTarget, Name: t.Name,
		Patch: patchWithRV(t.ResourceVersion, patch), Changes: ch,
		Warnings: append(gitopsWarning(t), s.warnings()...),
	}
	if s.URL != old.URL && old.URL != "" {
		a.Warnings = append(a.Warnings, "new URL: the backup volumes and backups of the old store disappear from the cluster (they stay in the store); new backups go to the new URL")
	}
	return a, nil
}

// DeleteTarget removes a target; Longhorn refuses to delete "default".
func DeleteTarget(t *BackupTarget, vols []*Volume) (*Action, error) {
	if t.Name == "default" {
		return nil, fmt.Errorf("Longhorn does not allow deleting the default backup target – set its URL to \"\" to disable it")
	}
	a := &Action{
		Title: fmt.Sprintf("Delete backup target %s (%s)", t.Name, t.Spec.BackupTargetURL),
		Verb:  VerbDelete, GVR: GVRBackupTarget, Name: t.Name, UID: string(t.UID), Destructive: true,
		Warnings: gitopsWarning(t),
	}
	var users []string
	for _, v := range vols {
		if v.Spec.BackupTargetName == t.Name {
			users = append(users, volKeyOf(v))
		}
	}
	if len(users) > 0 {
		a.Warnings = append(a.Warnings, fmt.Sprintf("%d volumes back up to this target and will fail until changed: %s", len(users), strings.Join(firstN(users, 8), ", ")))
	}
	a.Warnings = append(a.Warnings, "the backup volume and backup objects of this target are removed from the cluster; the data in the store stays")
	return a, nil
}

// SyncTarget asks Longhorn to re-read the store now (spec.syncRequestedAt),
// like the web UI's sync button – e.g. after another cluster wrote backups.
func SyncTarget(t *BackupTarget, now time.Time) *Action {
	ts := now.UTC().Format(time.RFC3339)
	return &Action{
		Title: "Sync backup target " + t.Name + " now",
		Verb:  VerbPatch, GVR: GVRBackupTarget, Name: t.Name,
		Patch:   patchWithRV(t.ResourceVersion, map[string]any{"syncRequestedAt": ts}),
		Changes: []Change{{"spec.syncRequestedAt", "", ts}},
	}
}

func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(slices.Clone(s[:n]), fmt.Sprintf("… %d more", len(s)-n))
}
