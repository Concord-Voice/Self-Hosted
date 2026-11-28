package hostagent

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// The update downgrade guard (#165). A compromised control plane can show an
// admin one action and have the key approve another; refusing an update to an
// older release keeps that from rolling the host back to a release with known
// flaws. The running release is the install's VERSION file, a plain x.y.z the
// release process keeps in step with the tag (see .github/scripts/verify-changelog.sh).

const maxVersionFileBytes = 64

type releaseVersion struct {
	core [3]uint64
	pre  []string // empty for a release
}

// parseReleaseVersion accepts vX.Y.Z or X.Y.Z with an optional -prerelease,
// the shape updateTagPattern allows.
func parseReleaseVersion(value string) (releaseVersion, error) {
	value = strings.TrimPrefix(value, "v")
	if !updateTagPattern.MatchString("v" + value) {
		return releaseVersion{}, errors.New("invalid release version")
	}
	core, pre, _ := strings.Cut(value, "-")
	var v releaseVersion
	for i, part := range strings.Split(core, ".") {
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return releaseVersion{}, fmt.Errorf("invalid release version: %w", err)
		}
		v.core[i] = n
	}
	if pre != "" {
		v.pre = strings.Split(pre, ".")
	}
	return v, nil
}

// compareReleaseVersions orders a and b by semantic-version precedence.
func compareReleaseVersions(a, b releaseVersion) int {
	for i := range a.core {
		if a.core[i] != b.core[i] {
			if a.core[i] < b.core[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a.pre) == 0 && len(b.pre) == 0:
		return 0
	case len(a.pre) == 0:
		return 1
	case len(b.pre) == 0:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		if c := comparePrerelease(a.pre[i], b.pre[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a.pre) < len(b.pre):
		return -1
	case len(a.pre) > len(b.pre):
		return 1
	}
	return 0
}

func comparePrerelease(a, b string) int {
	an, aErr := strconv.ParseUint(a, 10, 64)
	bn, bErr := strconv.ParseUint(b, 10, 64)
	switch {
	case aErr == nil && bErr == nil:
		if an == bn {
			return 0
		}
		if an < bn {
			return -1
		}
		return 1
	case aErr == nil: // numeric identifiers sort before alphanumeric ones
		return -1
	case bErr == nil:
		return 1
	}
	return strings.Compare(a, b)
}

// runningVersion reads the VERSION file under the same ownership rule as the
// backups folder: only root may say which release is running.
func (j *jobs) runningVersion() (releaseVersion, error) {
	if err := trustedEntry(filepath.Dir(j.version)); err != nil {
		return releaseVersion{}, fmt.Errorf("running version directory: %w", err)
	}
	st, err := os.Lstat(j.version)
	if err != nil {
		return releaseVersion{}, fmt.Errorf("inspect running version: %w", err)
	}
	if !st.Mode().IsRegular() || trustedEntry(j.version) != nil || st.Size() > maxVersionFileBytes {
		return releaseVersion{}, errors.New("running version file is untrusted")
	}
	f, err := os.OpenFile(j.version, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return releaseVersion{}, fmt.Errorf("open running version: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(f, maxVersionFileBytes+1))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || len(data) > maxVersionFileBytes {
		return releaseVersion{}, errors.New("read running version")
	}
	line, _, _ := strings.Cut(string(data), "\n")
	return parseReleaseVersion(strings.TrimSpace(line))
}

// refuseDowngrade refuses an update to a tag older than the running release.
// A running release it cannot read refuses too: the guard fails closed.
func (j *jobs) refuseDowngrade(tag string) *refusal {
	target, err := parseReleaseVersion(tag)
	if err != nil {
		return &refusal{http.StatusBadRequest, "invalid_tag"}
	}
	running, err := j.runningVersion()
	if err != nil {
		return &refusal{http.StatusConflict, "running_version_unknown"}
	}
	if compareReleaseVersions(target, running) < 0 {
		return &refusal{http.StatusConflict, "update_downgrade_refused"}
	}
	return nil
}
