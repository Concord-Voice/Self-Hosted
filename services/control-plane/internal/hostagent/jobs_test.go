package hostagent

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func post(t *testing.T, s *Server, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, target, strings.NewReader(body)))
	return rec
}

func writeServer(t *testing.T, f *fakeRunner) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	s := NewServer(&fakeRunner{out: statusJSON})
	s.EnableWrites(f, dir)
	return s, dir
}

// waitJob polls a job until it finishes.
func waitJob(t *testing.T, s *Server, id string) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec := get(t, s, http.MethodGet, "/v1/jobs/"+id)
		var j Job
		if err := json.Unmarshal(rec.Body.Bytes(), &j); err != nil {
			t.Fatalf("job %s: %d %s", id, rec.Code, rec.Body)
		}
		if j.State != "running" {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s still running", id)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func started(t *testing.T, rec *httptest.ResponseRecorder) Job {
	t.Helper()
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	var j Job
	if err := json.Unmarshal(rec.Body.Bytes(), &j); err != nil || !jobIDPattern.MatchString(j.ID) || j.State != "running" {
		t.Fatalf("bad job %s", rec.Body)
	}
	return j
}

func TestWritesOffByDefault(t *testing.T) {
	s := NewServer(&fakeRunner{})
	for _, p := range []string{"/v1/actions/start", "/v1/jobs"} {
		if rec := post(t, s, p, ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s: got %d", p, rec.Code)
		}
	}
}

func TestWriteActionsRunTheirCommands(t *testing.T) {
	cases := []struct {
		action, body string
		want         [][]string
	}{
		{"start", "", [][]string{{"up"}}},
		{"stop", "", [][]string{{"stop"}}},
		{"restart", "{}", [][]string{{"stop"}, {"up"}}},
		{"backup", "", [][]string{{"backup", "--json"}}},
		{"update", `{"tag":"v1.2.3-rc.1"}`, [][]string{{"update", "--to", "v1.2.3-rc.1", "--confirm", "--json"}}},
	}
	for _, c := range cases {
		f := &fakeRunner{}
		s, _ := writeServer(t, f)
		j := waitJob(t, s, started(t, post(t, s, "/v1/actions/"+c.action, c.body)).ID)
		if j.State != "succeeded" || j.Action != c.action || j.Finished == nil {
			t.Errorf("%s: %+v", c.action, j)
		}
		if !reflect.DeepEqual(f.args, c.want) {
			t.Errorf("%s: ran %v", c.action, f.args)
		}
	}
}

func TestRestoreTakesOnlyAnArchiveInBackups(t *testing.T) {
	f := &fakeRunner{}
	s, dir := writeServer(t, f)
	name := "concord-backup-20261106T071500Z.tar.gz"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, name), filepath.Join(dir, "concord-backup-20261106T071501Z.tar.gz")); err != nil {
		t.Fatal(err)
	}
	for body, want := range map[string]string{
		`{"archive":"concord-backup-20261106T071502Z.tar.gz"}`: "unknown_archive",
		`{"archive":"concord-backup-20261106T071501Z.tar.gz"}`: "unknown_archive", // a link
		`{"archive":"../concord-backup-20261106T071500Z.tar.gz"}`: "invalid_archive",
		`{"archive":"/etc/passwd"}`: "invalid_archive",
		`{}`: "invalid_archive",
		`{"archive":"` + name + `","tag":"v1.0.0"}`: "invalid_archive",
	} {
		rec := post(t, s, "/v1/actions/restore", body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: got %d %s", body, rec.Code, rec.Body)
		}
	}
	waitJob(t, s, started(t, post(t, s, "/v1/actions/restore", `{"archive":"`+name+`"}`)).ID)
	want := [][]string{{"restore", filepath.Join(dir, name), "--confirm", "--json"}}
	if !reflect.DeepEqual(f.args, want) {
		t.Errorf("ran %v", f.args)
	}
}

func TestRefusesBadWriteRequests(t *testing.T) {
	f := &fakeRunner{}
	s, _ := writeServer(t, f)
	for _, c := range []struct {
		action, body string
		code         int
		reason       string
	}{
		{"reboot", "", http.StatusNotFound, "unknown_action"},
		{"start/x", "", http.StatusNotFound, "unknown_action"},
		{"start", `{"tag":"v1.0.0"}`, http.StatusBadRequest, "unexpected_argument"},
		{"start", `{"cmd":"rm"}`, http.StatusBadRequest, "invalid_body"},
		{"start", `{}{}`, http.StatusBadRequest, "invalid_body"},
		{"start", `{"tag":"` + strings.Repeat("a", 2000) + `"}`, http.StatusBadRequest, "invalid_body"},
		{"update", `{"tag":"main"}`, http.StatusBadRequest, "invalid_tag"},
		{"update", `{"tag":"v1.0.0;id"}`, http.StatusBadRequest, "invalid_tag"},
		{"update", `{"tag":"--help"}`, http.StatusBadRequest, "invalid_tag"},
	} {
		rec := post(t, s, "/v1/actions/"+c.action, c.body)
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.reason) {
			t.Errorf("%s %s: got %d %s", c.action, c.body, rec.Code, rec.Body)
		}
	}
	if rec := get(t, s, http.MethodGet, "/v1/actions/start"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET action: got %d", rec.Code)
	}
	if rec := post(t, s, "/v1/jobs", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST jobs: got %d", rec.Code)
	}
	if len(f.args) != 0 {
		t.Errorf("refused requests ran %v", f.args)
	}
}

func TestOneWriteAtATime(t *testing.T) {
	f := &fakeRunner{wait: make(chan struct{})}
	s, _ := writeServer(t, f)
	first := started(t, post(t, s, "/v1/actions/stop", ""))
	if rec := post(t, s, "/v1/actions/start", ""); rec.Code != http.StatusConflict {
		t.Fatalf("second write: got %d", rec.Code)
	}
	// Reads still work while a write runs.
	if rec := get(t, s, http.MethodGet, "/v1/status"); rec.Code != http.StatusOK {
		t.Errorf("status during a write: got %d", rec.Code)
	}
	close(f.wait)
	waitJob(t, s, first.ID)
	waitJob(t, s, started(t, post(t, s, "/v1/actions/start", "")).ID)
}

func TestFailedStepStopsTheJob(t *testing.T) {
	f := &fakeRunner{out: "\x1b[31mstopping\x1b[0m\n", err: errors.New("exit 1")}
	s, _ := writeServer(t, f)
	j := waitJob(t, s, started(t, post(t, s, "/v1/actions/restart", "")).ID)
	if j.State != "failed" || len(f.args) != 1 {
		t.Errorf("state %s, ran %v", j.State, f.args)
	}
	if j.Output != "stopping\n" {
		t.Errorf("output %q", j.Output)
	}
}

func TestBackupJobReportsItsArchive(t *testing.T) {
	for out, want := range map[string]string{
		"dumping\n" + `{"schema":1,"ok":true,"archive":"concord-backup-20261106T071500Z.tar.gz"}` + "\n": "concord-backup-20261106T071500Z.tar.gz",
		`{"schema":1,"ok":true,"archive":"../x.tar.gz"}` + "\n":                                             "",
		"no json\n": "",
	} {
		s, _ := writeServer(t, &fakeRunner{out: out})
		if j := waitJob(t, s, started(t, post(t, s, "/v1/actions/backup", "")).ID); j.Archive != want {
			t.Errorf("%q: archive %q", out, j.Archive)
		}
	}
}

func TestJobsList(t *testing.T) {
	s, _ := writeServer(t, &fakeRunner{out: "x\n"})
	var ids []string
	for i := 0; i < maxJobs+2; i++ {
		ids = append(ids, waitJob(t, s, started(t, post(t, s, "/v1/actions/stop", "")).ID).ID)
	}
	var doc struct{ Jobs []Job }
	rec := get(t, s, http.MethodGet, "/v1/jobs")
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Jobs) != maxJobs || doc.Jobs[0].ID != ids[len(ids)-1] || doc.Jobs[0].Output != "" {
		t.Errorf("list: %d jobs, first %+v", len(doc.Jobs), doc.Jobs[0])
	}
	for _, id := range []string{ids[0], "nope", strings.Repeat("A", 32)} {
		if rec := get(t, s, http.MethodGet, "/v1/jobs/"+id); rec.Code != http.StatusNotFound {
			t.Errorf("%s: got %d", id, rec.Code)
		}
	}
}

func TestTailBufferKeepsWholeLines(t *testing.T) {
	tb := tailBuffer{max: 10}
	_, _ = tb.Write([]byte("aaaaaa\nbbb\ncc\n"))
	if got := tb.String(); got != "bbb\ncc\n" {
		t.Errorf("got %q", got)
	}
}
