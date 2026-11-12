package hostagent

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Backup list (#188) and appliance read-only mode.

const maxBackups = 100

// Appliances that run the containers themselves. On these the agent refuses
// every write: the appliance's own UI owns start, stop and updates.
var appliances = map[string]bool{"truenas": true}

// Backup is one archive in the install's backups folder.
type Backup struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// SetManagedBy marks the host as run by appliance, which turns writes off.
// It reports false, and changes nothing, for an appliance it does not know.
func (s *Server) SetManagedBy(appliance string) bool {
	if !appliances[appliance] {
		return false
	}
	s.managedBy = appliance
	s.writes = nil
	return true
}

// writePath reports whether path is one of the write-side endpoints, which
// an appliance-managed host refuses as a whole.
func writePath(path string) bool {
	return strings.HasPrefix(path, "/v1/actions/") || path == "/v1/jobs" ||
		strings.HasPrefix(path, "/v1/jobs/") || path == "/v1/backups"
}

func (s *Server) refuseManaged(w http.ResponseWriter) {
	w.Header().Set(contentType, "application/json")
	w.WriteHeader(http.StatusConflict)
	writeJSON(w, map[string]string{"error": "appliance_managed", "appliance": s.managedBy})
}

// handleBackups serves GET /v1/backups: the archives restore accepts,
// newest first.
func (s *Server) handleBackups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		refuse(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	list, no := s.writes.backupList()
	if no != nil {
		refuse(w, no.code, no.reason)
		return
	}
	writeJSON(w, map[string]any{"backups": list})
}

// backupList reads the backups folder. A folder that does not exist yet is
// an empty list; one root does not own is refused, as restore refuses it.
func (j *jobs) backupList() ([]Backup, *refusal) {
	st, err := os.Lstat(j.backups)
	if os.IsNotExist(err) {
		return []Backup{}, nil
	}
	if err != nil || !st.IsDir() || trustedEntry(j.backups) != nil {
		return nil, &refusal{http.StatusConflict, "untrusted_backups"}
	}
	entries, err := os.ReadDir(j.backups)
	if err != nil {
		return nil, &refusal{http.StatusServiceUnavailable, "backups_unavailable"}
	}
	list := []Backup{}
	for _, e := range entries {
		if !archivePattern.MatchString(e.Name()) || !e.Type().IsRegular() {
			continue
		}
		info, err := os.Lstat(filepath.Join(j.backups, e.Name()))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		list = append(list, Backup{Name: e.Name(), Size: info.Size(), Modified: info.ModTime().UTC()})
	}
	// The name carries the UTC time it was taken, so it sorts by age.
	sort.Slice(list, func(a, b int) bool { return list[a].Name > list[b].Name })
	if len(list) > maxBackups {
		list = list[:maxBackups]
	}
	return list, nil
}
