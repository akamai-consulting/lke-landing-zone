package clusterspec

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func intp(n int) *int { return &n }

// The schedule and KEEP are spec knobs now, not literals in a managed manifest.
// Each bound Validate enforces exists for a reason the error states; pin them.
func TestOpenBaoBackupKnobsValidate(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4, 6, 8, 12, 24} {
		if errs := validateComponentSizing("e", "openbaoBackup", ComponentToggle{BackupIntervalHours: intp(n)}); len(errs) != 0 {
			t.Errorf("backupIntervalHours %d refused: %v", n, errs)
		}
	}
	for _, n := range []int{0, -1, 5, 7, 9, 25, 48} {
		if errs := validateComponentSizing("e", "openbaoBackup", ComponentToggle{BackupIntervalHours: intp(n)}); len(errs) == 0 {
			t.Errorf("backupIntervalHours %d accepted; it is not a divisor of 24", n)
		}
	}
	for _, k := range []int{0, -3, OpenBaoBackupMaxKeep + 1} {
		if errs := validateComponentSizing("e", "openbaoBackup", ComponentToggle{BackupKeep: intp(k)}); len(errs) == 0 {
			t.Errorf("backupKeep %d accepted", k)
		}
	}
	if errs := validateComponentSizing("e", "harbor", ComponentToggle{BackupKeep: intp(3)}); len(errs) == 0 {
		t.Error("backupKeep on a component that does not read it was accepted")
	}
}

// The patch must reach the CronJob the component ships: same name, namespace and
// container, and the knob values (or the defaults) in the fields the producer
// reads. And the base manifest's own literals must equal the defaults, so a
// render with no knobs set and the bare component agree.
func TestOpenBaoBackupPatchTargetsTheShippedCronJob(t *testing.T) {
	base, err := os.ReadFile("../../../../platform-apl/components/openbaoBackup/openbao-backup/cronjob.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cj := string(base)
	for _, want := range []string{"name: openbao-backup", "namespace: llz-openbao-backup", "- name: backup",
		`schedule: "` + OpenBaoBackupSchedule(OpenBaoBackupDefaultIntervalHours) + `"`} {
		if !strings.Contains(cj, want) {
			t.Errorf("cronjob.yaml lacks %q — the rendered patch would not land on it, or the base default drifted", want)
		}
	}
	keepRe := regexp.MustCompile(`- name: KEEP\s+value: "(\d+)"`)
	m := keepRe.FindStringSubmatch(cj)
	if m == nil || m[1] != strconv.Itoa(OpenBaoBackupDefaultKeep) {
		t.Errorf("cronjob.yaml KEEP = %v, want the default %d", m, OpenBaoBackupDefaultKeep)
	}

	def := RenderOpenBaoBackupPatch(nil, nil)
	if !strings.Contains(def, `schedule: "23 */6 * * *"`) || !strings.Contains(def, `value: "28"`) {
		t.Errorf("default patch:\n%s", def)
	}
	got := RenderOpenBaoBackupPatch(intp(24), intp(14))
	for _, want := range []string{"name: openbao-backup", "namespace: llz-openbao-backup", "- name: backup",
		"- name: KEEP", `schedule: "23 0 * * *"`, `value: "14"`} {
		if !strings.Contains(got, want) {
			t.Errorf("patch for 24h/14 lacks %q:\n%s", want, got)
		}
	}
	if s := OpenBaoBackupSchedule(4); s != "23 */4 * * *" {
		t.Errorf("OpenBaoBackupSchedule(4) = %q", s)
	}
}

// The component declares exactly the patch render writes, targeting the CronJob.
func TestOpenBaoBackupComponentDeclaresItsPatch(t *testing.T) {
	for _, c := range Components {
		if c.Name != "openbaoBackup" {
			continue
		}
		if len(c.Patches) != 1 || c.Patches[0].Kind != "CronJob" || c.Patches[0].Name != "openbao-backup" {
			t.Fatalf("openbaoBackup patches = %+v", c.Patches)
		}
		return
	}
	t.Fatal("no openbaoBackup component")
}
