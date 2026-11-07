package hostagent

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupListShowsOnlyArchivesNewestFirst(t *testing.T) {
	s, dir := writeServer(t, &fakeRunner{})
	for _, name := range []string{
		"concord-backup-20261101T010203Z.tar.gz",
		"concord-backup-20261105T010203Z.tar.gz",
		"notes.txt",
		"concord-backup-bad.tar.gz",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A directory and a symlink with archive names are not archives.
	if err := os.Mkdir(filepath.Join(dir, "concord-backup-20261106T010203Z.tar.gz"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "concord-backup-20261107T010203Z.tar.gz")); err != nil {
		t.Fatal(err)
	}
	rec := get(t, s, http.MethodGet, "/v1/backups")
	var body struct{ Backups []Backup }
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if len(body.Backups) != 2 || body.Backups[0].Name != "concord-backup-20261105T010203Z.tar.gz" ||
		body.Backups[1].Name != "concord-backup-20261101T010203Z.tar.gz" || body.Backups[0].Size != 1 {
		t.Fatalf("got %+v", body.Backups)
	}
}

func TestBackupListMissingFolderIsEmptyAndUntrustedIsRefused(t *testing.T) {
	s, dir := writeServer(t, &fakeRunner{})
	s.writes.backups = filepath.Join(dir, "missing")
	if rec := get(t, s, http.MethodGet, "/v1/backups"); rec.Code != http.StatusOK || rec.Body.String() != "{\"backups\":[]}\n" {
		t.Fatalf("missing: got %d %s", rec.Code, rec.Body)
	}
	// A symlink in place of the folder is refused, as restore refuses it.
	link := filepath.Join(t.TempDir(), "backups")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	s.writes.backups = link
	if rec := get(t, s, http.MethodGet, "/v1/backups"); rec.Code != http.StatusConflict {
		t.Fatalf("untrusted: got %d %s", rec.Code, rec.Body)
	}
	if rec := post(t, s, "/v1/backups", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post: got %d", rec.Code)
	}
}

func TestApplianceManagedRefusesEveryWrite(t *testing.T) {
	s, _ := writeServer(t, &fakeRunner{})
	if s.SetManagedBy("someos") || s.managedBy != "" || s.writes == nil {
		t.Fatal("an unknown appliance changed the server")
	}
	if !s.SetManagedBy("truenas") {
		t.Fatal("truenas refused")
	}
	for _, p := range []string{"/v1/actions/stop", "/v1/jobs", "/v1/jobs/0123456789abcdef0123456789abcdef", "/v1/backups"} {
		rec := post(t, s, p, "")
		if rec.Code != http.StatusConflict || rec.Body.String() != "{\"appliance\":\"truenas\",\"error\":\"appliance_managed\"}\n" {
			t.Errorf("%s: got %d %s", p, rec.Code, rec.Body)
		}
	}
	// Reads still work.
	if rec := get(t, s, http.MethodGet, "/v1/status"); rec.Code != http.StatusOK {
		t.Errorf("status: got %d", rec.Code)
	}
}
