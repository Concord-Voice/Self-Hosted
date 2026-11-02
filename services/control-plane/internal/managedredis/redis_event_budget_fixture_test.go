//go:build integration || managedredis_integration

package managedredis_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/database"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedredis"
	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
)

// managedBudgetFixture owns only an isolated Docker server and its administrative
// client. Assertions own production clients/engines and their teardown ordering.
type managedBudgetFixture struct {
	endpoint        string
	container       string
	createAttempted bool
	admin           *redis.Client
	runtime         *managedredis.Runtime
	noExpiry        *managedredis.Runtime
	reader          *managedBudgetSecretReader
	credentials     map[uint64]managedredis.Credential
	clockMu         sync.RWMutex
	now             time.Time
	closeOnce       sync.Once
	closeErr        error
}

type managedBudgetSecretReader struct {
	mu         sync.RWMutex
	credential managedredis.Credential
}

func (r *managedBudgetSecretReader) ReadExact(ctx context.Context, scope managedredis.SecretScope) ([]byte, string, error) {
	r.mu.RLock()
	credential := r.credential
	r.mu.RUnlock()
	if ctx.Err() != nil || scope != credential.Scope {
		return nil, "", errors.New("managed budget fixture secret unavailable")
	}
	raw, err := json.Marshal(credential) // #nosec G117 -- synthetic fixture credentials are serialized only to the in-memory SecretReader consumer, never diagnostics.
	if err != nil {
		return nil, "", errors.New("managed budget fixture secret encoding failed")
	}
	return raw, scope.Name, nil
}

func (r *managedBudgetSecretReader) set(credential managedredis.Credential) {
	r.mu.Lock()
	r.credential = credential
	r.mu.Unlock()
}

func newManagedBudgetFixture(t *testing.T) *managedBudgetFixture {
	t.Helper()
	if os.Getenv("MANAGED_REDIS_INTEGRATION") != "1" {
		t.Skip("set MANAGED_REDIS_INTEGRATION=1 to run the managed budget fixture")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("managed budget fixture requires Docker")
	}
	f := &managedBudgetFixture{
		container:   "cv-ws-budget-" + managedBudgetRandom(t)[:16],
		now:         time.Now().UTC().Truncate(time.Second),
		credentials: make(map[uint64]managedredis.Credential),
	}
	scope := managedredis.SecretScope{
		Project: "budget-fixture", Environment: "test", Path: "/redis", Name: "BUDGET_FIXTURE_CREDENTIAL",
	}
	for _, generation := range []uint64{12, 13, 14, 99} {
		f.credentials[generation] = managedredis.Credential{
			Version: 1, Scope: scope, Service: "control-plane", Generation: generation,
			RotationID: fmt.Sprintf("budget-fixture-%d", generation),
			Username:   fmt.Sprintf("cv_cp_%d", generation), Password: managedBudgetRandom(t),
			ValidFrom: f.now.Add(-time.Minute), ValidUntil: f.now.Add(time.Hour),
		}
	}
	adminPassword := managedBudgetRandom(t)
	acl := f.aclContents(t, adminPassword)
	aclDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(aclDir, "users.acl"), []byte(acl), 0o600); err != nil {
		t.Fatal("managed budget fixture ACL file unavailable")
	}
	// Register after TempDir so container teardown precedes ACL-directory removal.
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err) // All retained fixture errors are fixed and nonsecret.
		}
	})
	f.start(t, aclDir, adminPassword)
	f.reader = &managedBudgetSecretReader{credential: f.credentials[12]}
	f.runtime = f.newRuntime(t, f.reader)
	f.noExpiry = f.newRuntime(t, &managedBudgetSecretReader{credential: f.credentials[99]})
	return f
}

func managedBudgetRandom(t *testing.T) string {
	t.Helper()
	var bytes [24]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		t.Fatal("managed budget fixture random material unavailable")
	}
	return hex.EncodeToString(bytes[:])
}

func managedBudgetPasswordHash(password string) string {
	// Redis ACL password hashes use SHA-256; these credentials exist only in
	// the disposable fixture and never enter OS command arguments or diagnostics.
	hash := sha256.Sum256([]byte(password))
	return "#" + hex.EncodeToString(hash[:])
}

func (f *managedBudgetFixture) aclContents(t *testing.T, adminPassword string) string {
	t.Helper()
	rule := managedredis.EventBudgetTestControlPlaneACLRule()
	tokens := strings.Fields(rule)
	withoutExpiry := make([]string, 0, len(tokens))
	removed := 0
	for _, token := range tokens {
		if token == "+pexpire" {
			removed++
			continue
		}
		withoutExpiry = append(withoutExpiry, token)
	}
	if removed != 1 {
		t.Fatal("managed budget fixture must remove exactly one PEXPIRE grant")
	}
	// The broad inspector is fixture-only, never an application identity. Its
	// privileges let independent assertions inspect expiry, clients and commands.
	lines := make([]string, 0, 6)
	lines = append(lines,
		"user default off resetpass -@all",
		"user budget_fixture_admin on resetpass "+managedBudgetPasswordHash(adminPassword)+" -@all +@all ~* &*",
	)
	for _, generation := range []uint64{12, 13, 14, 99} {
		credential := f.credentials[generation]
		state, grants := "on", rule
		if generation == 13 || generation == 14 {
			state = "off" // Publish enables the staged generation before Refresh.
		}
		if generation == 99 {
			grants = strings.Join(withoutExpiry, " ")
		}
		lines = append(lines, "user "+credential.Username+" "+state+" resetpass "+
			managedBudgetPasswordHash(credential.Password)+" -@all "+grants)
	}
	return strings.Join(lines, "\n") + "\n"
}

func (f *managedBudgetFixture) start(t *testing.T, aclDir, adminPassword string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	image := os.Getenv("MANAGED_REDIS_TEST_IMAGE")
	if image == "" {
		image = "redis:8-alpine"
	}
	if err := managedredis.ValidateRedisIntegrationImage(image); err != nil {
		t.Fatal("managed budget fixture image reference invalid")
	}
	// No host data volume or persistence is needed. The read-only ACL mount
	// follows the existing canary; this root-owned fixture does not prove UID custody.
	// #nosec G204 G702 -- fixed argv, generated name and TempDir mount; image passes ValidateRedisIntegrationImage so cannot be an option or shell text.
	create := exec.CommandContext(ctx, "docker", "create", "--rm", "--name", f.container,
		"--label", "concord.task=hosted2270-budget-managed",
		"--cpus", "0.5", "--memory", "128m", "-p", "127.0.0.1::6379",
		"-v", aclDir+":/acl:ro", "--entrypoint", "redis-server", image,
		"--aclfile", "/acl/users.acl", "--save", "", "--appendonly", "no")
	// A failed/timed-out CLI can still have created a stopped daemon resource.
	// Register the attempt before execution; --rm does not clean never-started containers.
	f.createAttempted = true
	if _, err := create.CombinedOutput(); err != nil {
		t.Fatal("managed budget fixture container creation failed")
	}
	// #nosec G204 -- fixed Docker argv targets only the fixture-generated container name; no shell.
	if _, err := exec.CommandContext(ctx, "docker", "start", f.container).CombinedOutput(); err != nil {
		t.Fatal("managed budget fixture container start failed")
	}
	portOutput, err := exec.CommandContext(ctx, "docker", "port", f.container, "6379/tcp").Output() // #nosec G204 -- fixed argv and fixture-generated name; no shell.
	if err != nil {
		t.Fatal("managed budget fixture endpoint unavailable")
	}
	host, port, err := net.SplitHostPort(strings.TrimSpace(string(portOutput)))
	if err != nil || host != "127.0.0.1" {
		t.Fatal("managed budget fixture endpoint is not loopback")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		t.Fatal("managed budget fixture port invalid")
	}
	f.endpoint = net.JoinHostPort(host, port)
	f.admin = redis.NewClient(&redis.Options{
		Addr: f.endpoint, Username: "budget_fixture_admin", Password: adminPassword,
		MaxRetries: -1, DialerRetries: 1, ContextTimeoutEnabled: true,
		DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, PoolTimeout: time.Second,
		MaintNotificationsConfig: &maintnotifications.Config{Mode: maintnotifications.ModeDisabled},
	})
	f.waitReady(ctx, t)
	f.logIdentity(ctx, t)
}

func (f *managedBudgetFixture) waitReady(parent context.Context, t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if f.admin.Ping(ctx).Err() == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("managed budget fixture Redis did not become ready")
		case <-ticker.C:
		}
	}
}

func (f *managedBudgetFixture) logIdentity(ctx context.Context, t *testing.T) {
	t.Helper()
	info, err := f.admin.Info(ctx, "server").Result()
	if err != nil {
		t.Fatal("managed budget fixture version unavailable")
	}
	version := ""
	for _, line := range strings.Split(info, "\n") {
		if value, found := strings.CutPrefix(strings.TrimSpace(line), "redis_version:"); found {
			version = value
		}
	}
	parts := strings.Split(version, ".")
	if len(parts) != 3 || parts[0] != "8" {
		t.Fatal("managed budget fixture requires a Redis 8 release")
	}
	for _, part := range parts {
		if _, err := strconv.ParseUint(part, 10, 32); err != nil {
			t.Fatal("managed budget fixture version invalid")
		}
	}
	imageOutput, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.Image}}", f.container).Output() // #nosec G204 -- fixed argv and generated fixture name; image output is validated before logging.
	if err != nil {
		t.Fatal("managed budget fixture image identity unavailable")
	}
	imageID := strings.TrimSpace(string(imageOutput))
	digest, ok := strings.CutPrefix(imageID, "sha256:")
	decoded, decodeErr := hex.DecodeString(digest)
	if !ok || decodeErr != nil || len(decoded) != sha256.Size {
		t.Fatal("managed budget fixture image identity invalid")
	}
	t.Logf("managed budget fixture Redis version=%s image=%s database=0", version, imageID)
}

func (f *managedBudgetFixture) newRuntime(t *testing.T, reader *managedBudgetSecretReader) *managedredis.Runtime {
	t.Helper()
	runtime, err := managedredis.NewRuntime(reader, reader.credential.Scope, "control-plane", f.Now)
	if err != nil {
		t.Fatal("managed budget fixture runtime construction failed")
	}
	runtime.SetCandidateProbe(func(ctx context.Context, candidate managedredis.Credential) error {
		return database.ProbeManagedRedisCandidate(ctx, f.endpoint, nil, candidate)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if err := runtime.Refresh(ctx); err != nil {
		t.Fatal("managed budget fixture initial credential probe failed")
	}
	return runtime
}

func (f *managedBudgetFixture) Endpoint() string { return f.endpoint }

func (f *managedBudgetFixture) Admin() *redis.Client { return f.admin }

func (f *managedBudgetFixture) Runtime() *managedredis.Runtime { return f.runtime }

func (f *managedBudgetFixture) NoExpiryRuntime() *managedredis.Runtime { return f.noExpiry }

func (f *managedBudgetFixture) Now() time.Time {
	f.clockMu.RLock()
	defer f.clockMu.RUnlock()
	return f.now
}

func (f *managedBudgetFixture) Advance(delta time.Duration) {
	f.clockMu.Lock()
	f.now = f.now.Add(delta)
	f.clockMu.Unlock()
}

// Publish enables the fixture user and stages its exact secret. The independent
// assertion owns Runtime.Refresh, including its real candidate-authentication probe.
func (f *managedBudgetFixture) Publish(ctx context.Context, generation uint64) error {
	credential, ok := f.credentials[generation]
	if !ok || generation == 99 || ctx.Err() != nil {
		return errors.New("managed budget fixture generation invalid")
	}
	if err := f.admin.Do(ctx, "ACL", "SETUSER", credential.Username, "on").Err(); err != nil {
		return errors.New("managed budget fixture generation enable failed")
	}
	f.reader.set(credential)
	return nil
}

func (f *managedBudgetFixture) Disable(ctx context.Context, generation uint64) error {
	credential, ok := f.credentials[generation]
	if !ok || ctx.Err() != nil {
		return errors.New("managed budget fixture generation invalid")
	}
	if err := f.admin.Do(ctx, "ACL", "SETUSER", credential.Username, "off").Err(); err != nil {
		return errors.New("managed budget fixture generation disable failed")
	}
	return nil
}

func (f *managedBudgetFixture) Close() error {
	f.closeOnce.Do(func() {
		if f.admin != nil {
			if err := f.admin.Close(); err != nil {
				f.closeErr = errors.New("managed budget fixture inspector cleanup failed")
			}
		}
		if f.createAttempted {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			// #nosec G204 -- fixed argv removes only the fixture-generated container name; no shell.
			if _, err := exec.CommandContext(ctx, "docker", "rm", "-f", f.container).CombinedOutput(); err != nil {
				// Tolerate only a successful daemon observation of exact-name
				// absence, not a CLI message that might instead mean daemon failure.
				// #nosec G204 -- fixed argv with an exact-name filter built only from the fixture's generated hexadecimal identifier.
				remaining, inspectErr := exec.CommandContext(ctx, "docker", "container", "ls", "--all",
					"--filter", "name=^/"+f.container+"$", "--format", "{{.Names}}").Output()
				if inspectErr != nil || strings.TrimSpace(string(remaining)) != "" {
					f.closeErr = errors.Join(f.closeErr, errors.New("managed budget fixture container cleanup failed"))
				}
			}
		}
	})
	return f.closeErr
}
