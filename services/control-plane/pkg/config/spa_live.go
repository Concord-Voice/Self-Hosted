// Package config — LiveSpaConfig provides hot-reloadable SPA configuration.
//
// When SPA_CONFIG_FILE is set, the control plane periodically re-reads that
// file. Operators can atomically replace it to update SPA configuration
// without rebuilding or restarting the service.
package config

import (
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SpaChannel selects a remote SPA stream. Pilot has no remote stream.
type SpaChannel string

// Remote SPA discovery supports exactly Stable and Pre-Release.
const (
	SpaStable     SpaChannel = "stable"
	SpaPrerelease SpaChannel = "prerelease"
)

// SpaSnapshot is one coherent deployment tuple. Version is audit metadata only.
type SpaSnapshot struct {
	URL         string
	IPCContract int
	Version     string
}

// Snapshot selects static configuration using the same defaults as live discovery.
func (c *Config) Snapshot(channel SpaChannel) SpaSnapshot {
	snapshot := SpaSnapshot{c.SpaURL, c.SpaIpcContract, c.SpaVersion}
	if channel == SpaPrerelease {
		snapshot = SpaSnapshot{c.SpaPrereleaseURL, c.SpaPrereleaseIpcContract, c.SpaPrereleaseVersion}
	}
	if !validSpaSnapshot(snapshot) {
		return SpaSnapshot{}
	}
	return snapshot
}

func validSpaSnapshot(s SpaSnapshot) bool {
	if s == (SpaSnapshot{}) {
		return true
	}
	if s.URL == "" || s.IPCContract <= 0 {
		return false
	}
	parsed, err := url.Parse(s.URL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Hostname() == "" || parsed.User != nil {
		return false
	}
	// Deployment labels are bounded opaque audit identifiers, not semver or authority.
	if len(s.Version) > 128 {
		return false
	}
	for _, ch := range s.Version {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-_+", ch) {
			return false
		}
	}
	return true
}

func parseSpaSnapshot(vars map[string]string, prefix string) (SpaSnapshot, bool) {
	s := SpaSnapshot{URL: vars[prefix+"URL"], Version: vars[prefix+"VERSION"]}
	rawContract := vars[prefix+"IPC_CONTRACT"]
	if rawContract != "" {
		contract, err := strconv.Atoi(rawContract)
		// Zero is the legacy disabled-stream spelling; tuple validation below
		// permits it only when URL and audit version are also empty.
		if err != nil || contract < 0 {
			return SpaSnapshot{}, false
		}
		s.IPCContract = contract
	}
	return s, validSpaSnapshot(s)
}

// LiveSpaConfig provides thread-safe, hot-reloadable SPA configuration.
// When a config file path is provided, it periodically re-reads the file
// to pick up changes without requiring a server restart.
type LiveSpaConfig struct {
	mu             sync.RWMutex
	spaURL         string
	spaIpcContract int
	spaVersion     string
	prerelease     SpaSnapshot
	filePath       string
	stopCh         chan struct{}
	stopOnce       sync.Once
}

// NewLiveSpaConfig creates a LiveSpaConfig initialized from the given Config.
// If filePath is non-empty, starts a background goroutine that re-reads
// the file every pollInterval.
func NewLiveSpaConfig(cfg *Config, filePath string, pollInterval time.Duration) *LiveSpaConfig {
	stable := cfg.Snapshot(SpaStable)
	lsc := &LiveSpaConfig{
		spaURL:         stable.URL,
		spaIpcContract: stable.IPCContract,
		spaVersion:     stable.Version,
		prerelease:     cfg.Snapshot(SpaPrerelease),
		filePath:       filePath,
		stopCh:         make(chan struct{}),
	}

	if filePath != "" {
		// Initial read from file (may override env var values with mounted file)
		lsc.reload()
		go lsc.watch(pollInterval)
		log.Printf("[SpaConfig] Hot-reload enabled")
	} else {
		log.Printf("[SpaConfig] Static mode")
	}

	return lsc
}

// Snapshot returns a complete selected tuple under one lock. Unknown channels
// default to Stable; an unavailable Pre-Release never falls back to Stable.
func (l *LiveSpaConfig) Snapshot(channel SpaChannel) SpaSnapshot {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if channel == SpaPrerelease {
		return l.prerelease
	}
	return SpaSnapshot{l.spaURL, l.spaIpcContract, l.spaVersion}
}

// SpaURL returns the Stable URL. Use Snapshot when reading multiple fields.
func (l *LiveSpaConfig) SpaURL() string { return l.Snapshot(SpaStable).URL }

// SpaIpcContract returns the Stable contract. Use Snapshot for coherent tuples.
func (l *LiveSpaConfig) SpaIpcContract() int { return l.Snapshot(SpaStable).IPCContract }

// Stop stops the background file watcher. Safe to call multiple times.
func (l *LiveSpaConfig) Stop() {
	l.stopOnce.Do(func() { close(l.stopCh) })
}

func (l *LiveSpaConfig) watch(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			l.reload()
		case <-l.stopCh:
			return
		}
	}
}

func (l *LiveSpaConfig) reload() {
	data, err := os.ReadFile(l.filePath)
	if err != nil {
		// File might not exist yet or be temporarily unavailable during git pull
		log.Printf("[SpaConfig] Warning: snapshot read failed; retaining previous configuration")
		return
	}

	vars, valid := parseSpaEnv(data)
	if !valid {
		log.Printf("[SpaConfig] Warning: malformed snapshot file; retaining previous configuration")
		return
	}
	stable, stableOK := parseSpaSnapshot(vars, "SPA_")
	prerelease, prereleaseOK := parseSpaSnapshot(vars, "SPA_PRERELEASE_")
	if !stableOK || !prereleaseOK {
		log.Printf("[SpaConfig] Warning: invalid snapshot; retaining previous configuration")
		return
	}

	// Validate the entire candidate off-lock, then publish both streams together.
	l.mu.Lock()
	defer l.mu.Unlock()
	l.spaURL = stable.URL
	l.spaIpcContract = stable.IPCContract
	l.spaVersion = stable.Version
	l.prerelease = prerelease
}

// parseSpaEnv rejects malformed or duplicate assignments rather than accepting
// a partial file as an intentional stream removal. parseDotenv remains the
// permissive legacy utility for unrelated callers.
func parseSpaEnv(data []byte) (map[string]string, bool) {
	vars := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
			return nil, false
		}
		key := strings.TrimSpace(parts[0])
		if _, exists := vars[key]; exists {
			return nil, false
		}
		vars[key] = strings.TrimSpace(parts[1])
	}
	return vars, true
}

// parseDotenv parses a simple KEY=VALUE file (ignoring comments and blank lines).
func parseDotenv(data []byte) map[string]string {
	result := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			result[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return result
}
