package hostagent

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestCompareReleaseVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"v1.2.3", "1.2.3", 0},
		{"v1.2.4", "1.2.3", 1},
		{"v1.10.0", "1.9.9", 1},
		{"v2.0.0", "1.99.99", 1},
		{"v0.3.9", "0.4.0", -1},
		{"v0.4.0-rc.1", "0.4.0", -1},
		{"v0.4.0-rc.2", "0.4.0-rc.10", -1},
		{"v0.4.0-alpha", "0.4.0-1", 1},
		{"v0.4.0-rc", "0.4.0-rc.1", -1},
		{"v0.4.1-rc.1", "0.4.0", 1},
	} {
		a, err := parseReleaseVersion(tc.a)
		require.NoError(t, err)
		b, err := parseReleaseVersion(tc.b)
		require.NoError(t, err)
		require.Equal(t, tc.want, compareReleaseVersions(a, b), "%s vs %s", tc.a, tc.b)
	}
	for _, bad := range []string{"", "1.2", "v1.2.3.4", "1.2.x", "v01.2.3-", "latest"} {
		_, err := parseReleaseVersion(bad)
		require.Error(t, err, bad)
	}
}

func TestUpdateDowngradeIsRefusedBeforeAnyKeyTap(t *testing.T) {
	runner := &fakeRunner{}
	s, _ := writeServer(t, runner)
	s.writes.version = writeVersionFile(t, "0.4.0")
	for _, tag := range []string{"v0.3.9", "v0.4.0-rc.1", "v0.1.0"} {
		rec := postBegin(t, s, "update", fmt.Sprintf(`{"tag":%q}`, tag))
		require.Equal(t, http.StatusConflict, rec.Code, tag)
		require.Contains(t, rec.Body.String(), "update_downgrade_refused", tag)
		require.NotContains(t, rec.Body.String(), "handle", "no ceremony is created for a downgrade")
	}
	for _, tag := range []string{"v0.4.0", "v0.4.1", "v1.0.0-rc.1"} {
		rec := postBegin(t, s, "update", fmt.Sprintf(`{"tag":%q}`, tag))
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", tag, rec.Body)
	}
	require.Empty(t, runner.args)
}

func TestUpdateDowngradeIsRefusedAgainAtStart(t *testing.T) {
	runner := &fakeRunner{}
	s, _ := writeServer(t, runner)
	s.writes.version = writeVersionFile(t, "0.4.0")
	list, err := s.authorizations.authorizers.List()
	require.NoError(t, err)
	handle, _, err := s.authorizations.Begin(list[0].AdminID, "update", actionRequest{Tag: "v0.4.1"})
	require.NoError(t, err)
	// The running release moves past the tag between the tap and the start.
	require.NoError(t, os.WriteFile(s.writes.version, []byte("0.5.0\n"), 0o644)) //nolint:gosec // test fixture
	rec := post(t, s, "/v1/actions/update", fmt.Sprintf(`{"handle":%q,"assertion":{"test":true}}`, handle))
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Contains(t, rec.Body.String(), "update_downgrade_refused")
	require.Empty(t, runner.args)
	lines := readAuditLines(t, s.audit.path)
	require.Len(t, lines, 1)
	require.Equal(t, "request_refused", lines[0]["reason_code"], "the assertion verified; the request was refused")
}

func TestUpdateRefusesWhenTheRunningVersionIsUnknown(t *testing.T) {
	s, _ := writeServer(t, &fakeRunner{})
	for name, prepare := range map[string]func(path string){
		"missing":   func(path string) { require.NoError(t, os.Remove(path)) },
		"garbage":   func(path string) { require.NoError(t, os.WriteFile(path, []byte("main\n"), 0o644)) },                 //nolint:gosec // test fixture
		"writable":  func(path string) { require.NoError(t, os.Chmod(path, 0o666)) },                                       //nolint:gosec // the unsafe mode under test
		"oversized": func(path string) { require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("1", 100)), 0o644)) }, //nolint:gosec // test fixture
		"symlink": func(path string) {
			target := writeVersionFile(t, "0.1.0")
			require.NoError(t, os.Remove(path))
			require.NoError(t, os.Symlink(target, path))
		},
	} {
		t.Run(name, func(t *testing.T) {
			s.writes.version = writeVersionFile(t, "0.1.0")
			prepare(s.writes.version)
			rec := postBegin(t, s, "update", `{"tag":"v9.9.9"}`)
			require.Equal(t, http.StatusConflict, rec.Code)
			require.Contains(t, rec.Body.String(), "running_version_unknown")
		})
	}
}

func TestEnableWritesReadsVersionBesideBackups(t *testing.T) {
	s := NewServer(&fakeRunner{})
	root := t.TempDir()
	s.EnableWrites(&fakeRunner{}, filepath.Join(root, "backups"), filepath.Join(t.TempDir(), "marker"))
	require.Equal(t, filepath.Join(root, "VERSION"), s.writes.version)
}

func TestUpdateSharesTheBackupAdmissionWindow(t *testing.T) {
	now := time.Date(2026, time.November, 26, 12, 0, 0, 0, time.UTC)
	for _, order := range [][2]string{{"backup", "update"}, {"update", "backup"}, {"update", "update"}} {
		t.Run(order[0]+" then "+order[1], func(t *testing.T) {
			runner := &fakeRunner{}
			s, _ := writeServer(t, runner)
			s.writes.now = func() time.Time { return now }
			args := map[string]string{"backup": "", "update": `{"tag":"v1.2.3"}`}
			first := started(t, postAuthorized(t, s, "/v1/actions/"+order[0], args[order[0]]))
			waitJob(t, s, first.ID)
			rec := postAuthorized(t, s, "/v1/actions/"+order[1], args[order[1]])
			require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), "backup_rate_limited")
			require.Len(t, runner.args, 1, "the second action never ran")
		})
	}
	// Actions that take no backup are not limited by one.
	s, _ := writeServer(t, &fakeRunner{})
	s.writes.now = func() time.Time { return now }
	waitJob(t, s, started(t, postAuthorized(t, s, "/v1/actions/update", `{"tag":"v1.2.3"}`)).ID)
	waitJob(t, s, started(t, postAuthorized(t, s, "/v1/actions/stop", "")).ID)
}

func TestAuthorizerUnavailableAtConsumeIsNotAnAssertionDenial(t *testing.T) {
	runner := &fakeRunner{}
	s, _ := writeServer(t, runner)
	list, err := s.authorizations.authorizers.List()
	require.NoError(t, err)
	handle, _, err := s.authorizations.Begin(list[0].AdminID, "stop", actionRequest{})
	require.NoError(t, err)
	// An unsafe trust file makes verification impossible, not failed.
	require.NoError(t, os.Chmod(s.authorizations.authorizers.path, 0o666)) //nolint:gosec // the unsafe mode under test
	rec := post(t, s, "/v1/actions/stop", fmt.Sprintf(`{"handle":%q,"assertion":{"test":true}}`, handle))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "authorizer_unavailable")
	lines := readAuditLines(t, s.audit.path)
	require.Len(t, lines, 1)
	require.Equal(t, "authorizer_unavailable", lines[0]["reason_code"])
	require.Empty(t, runner.args)
}

func TestRequestRefusedAfterConsumeIsNotAnAssertionDenial(t *testing.T) {
	s, dir := writeServer(t, &fakeRunner{})
	name := "concord-backup-20261106T071500Z.tar.gz"
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("backup"), 0o600))
	list, err := s.authorizations.authorizers.List()
	require.NoError(t, err)
	handle, _, err := s.authorizations.Begin(list[0].AdminID, "restore", actionRequest{Archive: name})
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))
	rec := post(t, s, "/v1/actions/restore", fmt.Sprintf(`{"handle":%q,"assertion":{"test":true}}`, handle))
	require.Equal(t, http.StatusNotFound, rec.Code)
	lines := readAuditLines(t, s.audit.path)
	require.Len(t, lines, 1)
	require.Equal(t, "request_refused", lines[0]["reason_code"])
	require.Equal(t, list[0].AdminID, lines[0]["admin_id"])
}

// TestDenialFloodCannotBlockOrEvictAdmission floods the two denial routes the
// allowed peer can reach with distinct and repeated keys, then requires an
// admission to succeed with its record in the stream beside the earlier one.
func TestDenialFloodCannotBlockOrEvictAdmission(t *testing.T) {
	s, _ := writeServer(t, &fakeRunner{})
	now := time.Date(2026, time.November, 26, 12, 0, 0, 0, time.UTC)
	s.denials.now = func() time.Time { return now }
	first := started(t, postAuthorized(t, s, "/v1/actions/stop", ""))
	waitJob(t, s, first.ID)
	for i := range 3000 {
		admin := uuid.NewString()
		if i%2 == 0 {
			admin = "6f1d8c1e-2c5b-4c7e-9a7e-0f6d1b2a3c4d"
		}
		post(t, s, "/v1/authorizations", fmt.Sprintf(`{"admin_id":%q,"action":"stop","arguments":{"tag":"x"}}`, admin))
		post(t, s, "/v1/actions/stop", `{"handle":"forged","assertion":{"a":1}}`)
	}
	denials := 0
	for _, line := range readAuditLines(t, s.audit.path) {
		if line["outcome"] == "denied" {
			denials++
		}
	}
	require.LessOrEqual(t, denials, maxDenialKeys+4, "6000 denials coalesce to a bounded set of records")

	second := started(t, postAuthorized(t, s, "/v1/actions/stop", ""))
	waitJob(t, s, second.ID)
	admitted := map[string]bool{}
	for _, line := range readAuditLines(t, s.audit.path) {
		if line["outcome"] == "admitted" {
			jobID, ok := line["job_id"].(string)
			require.True(t, ok, "an admitted record carries a string job_id")
			admitted[jobID] = true
		}
	}
	require.True(t, admitted[first.ID] && admitted[second.ID], "both admitted records are in the stream")
}

func TestDenialsCannotUseTheAdmissionReserve(t *testing.T) {
	// Denials stop below the 19M logrotate size, so they can never force a
	// rotation that evicts admitted records, and leave room for admissions.
	require.Less(t, maxAuditDenialBytes, 19<<20)
	require.Less(t, maxAuditDenialBytes, maxAuditFileBytes)

	logs := captureLog(t)
	runner := &fakeRunner{}
	s, _ := writeServer(t, runner)
	require.NoError(t, os.Truncate(s.audit.path, maxAuditDenialBytes))
	require.ErrorIs(t, s.audit.Write(AuditEvent{Outcome: "denied", Reason: "begin_denied"}), errAuditDenialReserve)
	require.ErrorIs(t, s.audit.Write(AuditEvent{Outcome: "denied", Reason: "begin_denied"}), errAuditDenialReserve)
	require.Equal(t, 1, strings.Count(logs.String(), "denials are no longer recorded"), "reported once")
	require.NotContains(t, logs.String(), "host-agent audit full;", "the stream itself is not full")

	job := started(t, postAuthorized(t, s, "/v1/actions/stop", ""))
	require.Equal(t, "succeeded", waitJob(t, s, job.ID).State, "admission still has its reserve")
	require.Equal(t, [][]string{{"stop"}}, runner.args)
}
