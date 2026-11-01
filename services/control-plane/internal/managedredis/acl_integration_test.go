//go:build integration || managedredis_integration

package managedredis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// This opt-in test requires Docker and runs only a task-owned, disposable Redis.
// It validates Redis ACL semantics; it does not prove production UID or mount custody.
func TestRedisACLIntegration(t *testing.T) {
	if os.Getenv("MANAGED_REDIS_INTEGRATION") != "1" {
		t.Skip("set MANAGED_REDIS_INTEGRATION=1 to run the real Redis ACL canary")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("Docker CLI unavailable")
	}
	name := fmt.Sprintf("cv-redis-acl-%d", time.Now().UnixNano())
	work := t.TempDir()
	data := filepath.Join(work, "data")
	aclDir := filepath.Join(work, "acl")
	require.NoError(t, os.MkdirAll(data, 0o700))
	require.NoError(t, os.MkdirAll(aclDir, 0o700))
	aclPath := filepath.Join(aclDir, "users.acl")
	passwords := map[string]string{
		"cv_cp_11": "cp-old-fixture", "cv_cp_12": "cp-new-fixture",
		"cv_mp_11": "mp-old-fixture", "cv_mp_12": "mp-new-fixture",
	}
	policy := integrationPolicy(passwords)
	initial := policy
	initial.RotationID = policy.RotationID
	initial.Services = make(map[string]PolicyService, len(policy.Services))
	for service, value := range policy.Services {
		value.TargetGeneration = 11
		value.Generations = append([]PolicyGeneration(nil), policy.Services[service].Generations[:1]...)
		initial.Services[service] = value
	}
	now := policyTestNow
	admin := &integrationAdminFixture{password: "admin-fixture"}
	stateDir := filepath.Join(work, "state")
	require.NoError(t, os.Mkdir(stateDir, 0o700))
	controller, err := NewController(ControllerOptions{
		Admin: admin, ACLPath: aclPath, JournalPath: filepath.Join(stateDir, "rotation.json"),
		MarkerPath: testMarkerPath(t, filepath.Join(stateDir, "rotation.json")),
		Bootstrap:  AdminBootstrap{Version: 1, Scope: SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_ADMIN_BOOTSTRAP"}, Username: "cv_admin", Password: admin.password},
		Now:        func() time.Time { return now }, ExpectedUIDs: map[string]uint32{"control-plane": 1002, "media-plane": 1003},
	})
	require.NoError(t, err)
	require.NoError(t, controller.PrepareInitial(initial))
	contents, err := os.ReadFile(aclPath) // #nosec G304 -- fixed users.acl basename beneath this test's t.TempDir, also passed to the fixture controller.
	require.NoError(t, err)
	manifest := checkedACLManifest
	require.NotContains(t, string(contents), "+@all")
	require.NotContains(t, string(contents), "~*")

	// The fixture runs as root because the test-created host bind mount's UID mapping
	// is not equivalent to the production tmpfs ownership contract.
	image := os.Getenv("MANAGED_REDIS_TEST_IMAGE")
	if image == "" {
		image = "redis:7-alpine"
	}
	require.NoError(t, ValidateRedisIntegrationImage(image))
	// #nosec G204 G702 -- fixed argv, generated name and TempDir mounts; image passes ValidateRedisIntegrationImage so cannot be an option or shell text.
	run := exec.CommandContext(ctx, "docker", "run", "-d", "--rm", "--name", name,
		"--label", "concord.task=epic2944-3930-acl-integration",
		"--cpus", "0.5", "--memory", "128m", "-p", "127.0.0.1::6379",
		"-v", aclDir+":/acl:ro", "-v", data+":/data",
		"--entrypoint", "redis-server", image, "--aclfile", "/acl/users.acl", "--dir", "/data", "--appendonly", "yes")
	out, err := run.CombinedOutput()
	require.NoErrorf(t, err, "start fixture: %s", out)
	t.Cleanup(func() {
		_ = exec.Command("docker", "exec", name, "chmod", "-R", "a+rwX", "/data").Run() // #nosec G204 -- fixed argv and generated fixture name; chmod is confined to its disposable /data mount.
		_ = exec.Command("docker", "rm", "-f", name).Run()                              // #nosec G204 -- fixed argv removes only the generated fixture container; no shell.
	})
	portOut, err := exec.Command("docker", "port", name, "6379/tcp").Output() // #nosec G204 -- fixed argv and generated fixture name; no shell.
	require.NoError(t, err)
	endpoint := strings.TrimSpace(string(portOut))
	_, port, err := net.SplitHostPort(strings.TrimPrefix(endpoint, "127.0.0.1:"))
	if err != nil { // Docker commonly prints 127.0.0.1:<port>.
		_, port, err = net.SplitHostPort(endpoint)
	}
	require.NoError(t, err)
	addr := net.JoinHostPort("127.0.0.1", port)
	adminClient := integrationRedisClient(t, addr, "cv_admin", admin.password)
	t.Cleanup(func() { _ = adminClient.Close() })
	admin.client = adminClient
	require.Eventually(t, func() bool { return adminClient.Ping(ctx).Err() == nil }, 20*time.Second, 100*time.Millisecond)
	versionOut, err := exec.CommandContext(ctx, "docker", "exec", name, "redis-server", "--version").Output() // #nosec G204 -- fixed version command targets only the generated fixture name; no shell.
	require.NoError(t, err)
	aclDigest := sha256.Sum256(contents)
	t.Logf("Redis fixture image=%s version=%s ACL_SHA256=%s command_counts={control-plane:%d,media-plane:%d} key_pattern_counts={control-plane:%d,media-plane:%d} container_user=root", image, strings.TrimSpace(string(versionOut)), hex.EncodeToString(aclDigest[:]), len(manifest.Services["control-plane"].Commands), len(manifest.Services["media-plane"].Commands), len(manifest.Services["control-plane"].KeyPatterns), len(manifest.Services["media-plane"].KeyPatterns))
	if err := controller.Reconcile(ctx, policy); err != nil {
		t.Fatalf("reconcile: %v; ACL load: %v", err, admin.loadACLErr)
	}

	// Every manifest permission is exercised by ACL DRYRUN with its declared key
	// family, including representative command arities and both read/write paths.
	checker := diagnosticClient(adminClient)
	t.Cleanup(func() { _ = checker.Close() })
	for service, selector := range manifest.Services {
		user := policy.Services[service].Generations[1].Username
		client := integrationRedisClient(t, addr, user, passwords[user])
		t.Cleanup(func() { _ = client.Close() })
		require.NoError(t, client.Ping(ctx).Err())
		for _, key := range selector.KeyPatterns {
			key = strings.TrimSuffix(key, "*") + "acl-fixture"
			_, err := checker.Do(ctx, "ACL", "DRYRUN", user, "GET", key).Result()
			require.NoErrorf(t, err, "%s GET %s", service, key)
		}
		for _, command := range selector.Commands {
			args := integrationDryRunArgs(command, selector.KeyPatterns[0])
			_, err := checker.Do(ctx, append([]any{"ACL", "DRYRUN", user}, args...)...).Result()
			require.NoErrorf(t, err, "%s command %s", service, command)
		}
		for _, command := range []string{"CONFIG", "ACL", "CLIENT"} {
			_, err := client.Do(ctx, command, "GET", "*", "NO_SUCH_SUBCOMMAND").Result()
			require.Error(t, err, service+" must not administer Redis")
		}
	}
	for _, keyPattern := range manifest.BluegreenOperator.KeyPatterns {
		key := strings.TrimSuffix(keyPattern, "*") + "acl-fixture"
		_, err := checker.Do(ctx, "ACL", "DRYRUN", "cv_admin", "GET", key).Result()
		require.NoErrorf(t, err, "operator GET %s", key)
	}
	for _, command := range manifest.BluegreenOperator.Commands {
		args := integrationOperatorDryRunArgs(command)
		_, err := checker.Do(ctx, append([]any{"ACL", "DRYRUN", "cv_admin"}, args...)...).Result()
		require.NoErrorf(t, err, "operator command %s", command)
	}
	cp := integrationRedisClient(t, addr, "cv_cp_12", passwords["cv_cp_12"])
	mp := integrationRedisClient(t, addr, "cv_mp_12", passwords["cv_mp_12"])
	t.Cleanup(func() { _ = cp.Close(); _ = mp.Close() })
	// TIME is needed by the actual account-budget Lua body, and remains outside
	// the media and operator grants. Check nested execution rather than only
	// the manifest or ACL DRYRUN handshake.
	_, err = cp.Time(ctx).Result()
	require.NoError(t, err)
	_, err = mp.Time(ctx).Result()
	require.ErrorContains(t, err, "NOPERM")
	_, err = adminClient.Time(ctx).Result()
	require.ErrorContains(t, err, "NOPERM", "operator must not inherit the control-plane TIME grant")
	_, err = mp.Eval(ctx, `return redis.call('TIME')`, []string{"voice:room_owner:acl-fixture"}).Result()
	requireBudgetTimeACLRefusal(t, err, "cv_mp_12")
	budgetSource, err := os.ReadFile(filepath.Join("..", "websocket", "event_admission.lua"))
	require.NoError(t, err)
	budgetKey := "ratelimit:user:ws:v1:{11111111-1111-4111-8111-111111111111}:profile"
	t.Run("account script refuses before TIME grant", func(t *testing.T) {
		assertBudgetScriptRequiresTimeGrant(ctx, t, aclPath, admin, cp, string(budgetSource), budgetKey)
	})
	budgetReply, err := cp.Eval(ctx, string(budgetSource), []string{budgetKey}, "1", "5000000", "8").Result()
	require.NoError(t, err)
	require.Equal(t, []interface{}{int64(1), int64(1), int64(0)}, budgetReply)
	budgetFields, err := cp.HGetAll(ctx, budgetKey).Result()
	require.NoError(t, err)
	require.Len(t, budgetFields, 3)
	require.Equal(t, "1", budgetFields["v"])
	require.NoError(t, cp.Del(ctx, budgetKey).Err())
	for _, key := range []string{"mfa_upgrade_bypass:acl-fixture", "session_revokes:acl-fixture", "attach_sess:acl-fixture"} {
		require.NoError(t, cp.Set(ctx, key, "preserve", 0).Err(), key)
		require.Equal(t, "preserve", cp.Get(ctx, key).Val())
	}
	require.Error(t, mp.Set(ctx, "voice:room_owner:acl-fixture", "x", 0).Err())
	require.Error(t, mp.Set(ctx, "media:colour_draining:blue", "x", 0).Err())
	require.NoError(t, adminClient.Set(ctx, "voice:room_owner:acl-fixture", "owner-fixture", 0).Err())
	require.NoError(t, mp.Get(ctx, "voice:room_owner:acl-fixture").Err())
	require.NoError(t, mp.MGet(ctx, "media:colour_draining:blue", "voice:room_owner:acl-fixture").Err())
	require.NoError(t, cp.Eval(ctx, `return redis.call('GET', KEYS[1])`, []string{"session_revokes:acl-fixture"}).Err())
	// Exercise the token theft fallback and cleanup scripts with the deployed ACL.
	fallbackKey := "token_theft_pending_revocations:fallback:v2"
	markerKey := "token_theft_blocked:acl-fixture"
	conn := cp.Conn()
	require.NoError(t, conn.Eval(ctx, `
redis.call("HSET", KEYS[1], ARGV[1], ARGV[2])
redis.call("SET", KEYS[2], ARGV[2])
return 1`, []string{fallbackKey, markerKey}, "acl-fixture", "operation-fixture").Err())
	local, err := conn.WaitAOF(ctx, 1, 0, 3*time.Second).Result()
	require.NoError(t, err)
	require.Equal(t, []int64{1, 0}, local)
	require.NoError(t, conn.Close())
	require.Equal(t, "operation-fixture", cp.HGet(ctx, fallbackKey, "acl-fixture").Val())
	require.Equal(t, "operation-fixture", cp.Get(ctx, markerKey).Val())
	require.NoError(t, cp.HGetAll(ctx, fallbackKey).Err())
	require.NoError(t, cp.SMembers(ctx, "token_theft_pending_revocations:fallback").Err())
	require.NoError(t, cp.Eval(ctx, `
if redis.call("HGET", KEYS[1], ARGV[1]) == ARGV[2] then
  redis.call("HDEL", KEYS[1], ARGV[1])
end
if redis.call("GET", KEYS[2]) == ARGV[2] then
  redis.call("DEL", KEYS[2])
end
return 1`, []string{fallbackKey, markerKey}, "acl-fixture", "operation-fixture").Err())
	require.Equal(t, int64(0), cp.Exists(ctx, markerKey).Val())

	// A real overlapping ACL load preserves the keyspace and drops old AUTH only
	// after exact acknowledgments and an observed zero-client drain.
	old := integrationRedisClient(t, addr, "cv_cp_11", passwords["cv_cp_11"])
	t.Cleanup(func() { _ = old.Close() })
	require.NoError(t, old.Ping(ctx).Err())
	require.NoError(t, controller.RegisterExpected(ctx, "rotation-12", map[string][]string{"control-plane": {"cp-fixture"}, "media-plane": {"mp-fixture"}}, map[string]uint64{"control-plane": 12, "media-plane": 12}))
	require.NoError(t, controller.BindInstance(ctx, "control-plane", fmt.Sprintf("%064x", 1), "cp-fixture", ""))
	require.NoError(t, controller.BindInstance(ctx, "media-plane", fmt.Sprintf("%064x", 2), "mp-fixture", ""))
	require.NoError(t, controller.RecordAck(ctx, "control-plane", 1002, AppliedAck{Version: 1, RotationID: "rotation-12", Service: "control-plane", InstanceID: "cp-fixture", Generation: 12, Username: "cv_cp_12"}))
	require.NoError(t, controller.RecordAck(ctx, "media-plane", 1003, AppliedAck{Version: 1, RotationID: "rotation-12", Service: "media-plane", InstanceID: "mp-fixture", Generation: 12, Username: "cv_mp_12"}))
	// Close the known old socket so the controller observes zero old connections.
	require.NoError(t, old.Close())
	now = now.Add(31 * time.Second)
	aclBeforeFinalize, err := os.Stat(aclPath)
	require.NoError(t, err)
	require.NoError(t, controller.Finalize(ctx, "rotation-12", map[string]string{"control-plane": "cv_cp_11", "media-plane": "cv_mp_11"}))
	aclAfterFinalize, err := os.Stat(aclPath)
	require.NoError(t, err)
	require.False(t, os.SameFile(aclBeforeFinalize, aclAfterFinalize), "finalization must atomically replace the ACL file")
	remainingClients, err := admin.ListClients(ctx)
	require.NoError(t, err)
	for _, connected := range remainingClients {
		require.NotContains(t, []string{"cv_cp_11", "cv_mp_11"}, connected.Username)
	}
	failedOld := integrationRedisClient(t, addr, "cv_cp_11", passwords["cv_cp_11"])
	require.Error(t, failedOld.Ping(ctx).Err())
	_ = failedOld.Close()
	require.NoError(t, cp.Set(ctx, "session_revokes:acl-fixture", "after-finalize", 0).Err())

	// Restart from the committed ACL file and AOF; both new auth and data survive.
	require.NoError(t, cp.Set(ctx, "session_revokes:acl-fixture", "restart-proof", 0).Err())
	require.NoError(t, exec.CommandContext(ctx, "docker", "restart", name).Run())        // #nosec G204 -- fixed argv restarts only the generated fixture container; no shell.
	portOut, err = exec.CommandContext(ctx, "docker", "port", name, "6379/tcp").Output() // #nosec G204 -- fixed argv inspects only the generated fixture container; no shell.
	require.NoError(t, err)
	addr = net.JoinHostPort("127.0.0.1", dockerPortNumber(t, string(portOut)))
	_ = cp.Close()
	_ = mp.Close()
	cp = integrationRedisClient(t, addr, "cv_cp_12", passwords["cv_cp_12"])
	mp = integrationRedisClient(t, addr, "cv_mp_12", passwords["cv_mp_12"])
	admin.client = integrationRedisClient(t, addr, "cv_admin", admin.password)
	if !eventually(20*time.Second, 100*time.Millisecond, func() bool { return cp.Ping(ctx).Err() == nil }) {
		logs, _ := exec.CommandContext(ctx, "docker", "logs", name).CombinedOutput() // #nosec G204 -- fixed argv reads only the generated fixture container's logs; no shell.
		t.Fatalf("Redis did not recover after restart: %s", logs)
	}
	require.Equal(t, "restart-proof", cp.Get(ctx, "session_revokes:acl-fixture").Val())
	require.NoError(t, controller.ProbeConfigured(ctx))
	roomID := "01234567-89ab-cdef-0123-456789abcdef"
	ownerKey := "voice:room_owner:" + roomID
	liveKey := "voice:room_live:" + roomID
	drainKey := "media:colour_draining:blue"
	require.NoError(t, admin.client.Set(ctx, ownerKey, "blue", 0).Err())
	createScript := readMediaLuaScript(t, "CREATE_ROOM_LIVE_WITH_FENCE_SCRIPT")
	refreshScript := readMediaLuaScript(t, "REFRESH_ROOM_LIVE_WITH_FENCE_SCRIPT")
	removeScript := readMediaLuaScript(t, "REMOVE_OWNED_ROOM_LIVE_SCRIPT")
	createKeys := []string{ownerKey, liveKey, drainKey}
	createArgs := []interface{}{"blue", "fixture-instance", "90"}
	flat := integrationRedisClient(t, addr, "cv_mp_flat", passwords["cv_mp_12"])
	t.Cleanup(func() { _ = flat.Close() })
	_, err = flat.Eval(ctx, createScript, createKeys, createArgs...).Result()
	require.Error(t, err, "an unpaired flat EVAL grant must fail the mixed owner/live/drain key vector")
	require.Contains(t, strings.ToUpper(err.Error()), "NOPERM")
	created, err := mp.Eval(ctx, createScript, createKeys, createArgs...).Int()
	require.NoError(t, err)
	require.Equal(t, 1, created)
	refreshed, err := mp.Eval(ctx, refreshScript, []string{ownerKey, liveKey}, createArgs...).Int()
	require.NoError(t, err)
	require.Equal(t, 1, refreshed)
	removed, err := mp.Eval(ctx, removeScript, []string{liveKey}, "fixture-instance").Int()
	require.NoError(t, err)
	require.Equal(t, 1, removed)
	for _, protectedKey := range []string{ownerKey, drainKey} {
		_, err := mp.Eval(ctx, `return redis.call('SET', KEYS[1], ARGV[1])`, []string{protectedKey}, "forbidden").Result()
		require.Error(t, err, "the EVAL selector must not grant nested SET to %s", protectedKey)
		require.Contains(t, strings.ToUpper(err.Error()), "NO PERMISSIONS TO ACCESS A KEY")
	}
	require.Equal(t, "blue", admin.client.Get(ctx, ownerKey).Val(), "blocked Lua must leave owner state unchanged")
	require.ErrorIs(t, admin.client.Get(ctx, drainKey).Err(), redis.Nil, "blocked Lua must leave drain state absent")
}

type integrationAdminFixture struct {
	client     *redis.Client
	password   string
	loadACLErr error
}

// ValidateRedisIntegrationImage checks the image argument shared by the two
// opt-in Docker fixtures. It is test-only, including for the external test
// package, and excludes CLI options, whitespace, controls and shell syntax.
func ValidateRedisIntegrationImage(image string) error {
	// Keep registry ports, bracketed IPv6 hosts, repository paths, tags and
	// digest references usable. This guarantees a single image argument, not
	// complete OCI validation, image trust or digest pinning. Docker validates
	// the full reference; the test runner chooses the executable image.
	const domainPart = `(?:[A-Za-z0-9]|[A-Za-z0-9][A-Za-z0-9-]*[A-Za-z0-9])`
	const domain = `(?:` + domainPart + `(?:\.` + domainPart + `)*|\[[A-Fa-f0-9:]+\])(?::[0-9]+)?`
	const component = `[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*`
	const repository = `(?:` + domain + `/)?` + component + `(?:/` + component + `)*`
	const tag = `(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?`
	const digest = `(?:@[A-Za-z][A-Za-z0-9]*(?:[-_+.][A-Za-z][A-Za-z0-9]*)*:[A-Fa-f0-9]{32,})?`
	if !regexp.MustCompile(`^` + repository + tag + digest + `$`).MatchString(image) {
		return errors.New("Redis integration fixture image reference is invalid")
	}
	return nil
}

func TestValidateRedisIntegrationImage(t *testing.T) {
	for _, image := range []string{
		"redis", "redis:8-alpine", "docker.io/library/redis:8.10.2",
		"localhost:5000/team/redis:fixture_1", "[::1]:5000/team/redis:8",
		"registry.example/team/redis@sha256:" + strings.Repeat("a", 64),
		"registry.example/team/redis:8@sha256:" + strings.Repeat("b", 64),
	} {
		require.NoError(t, ValidateRedisIntegrationImage(image))
	}
	for _, image := range []string{
		"", "--privileged", "-v/tmp:/data", "privileged=true", "redis --privileged",
		" redis:8", "redis:8 ", "redis\t8", "redis\n8", "redis\r8", "redis\x008",
		"$(command)", "redis;command", "redis@sha256:", "https://registry.example/redis",
	} {
		require.EqualError(t, ValidateRedisIntegrationImage(image), "Redis integration fixture image reference is invalid")
	}
}

func (a *integrationAdminFixture) LoadACL(ctx context.Context, path string) error {
	if err := appendACLDiagnosticUserOnDisk(path); err != nil {
		a.loadACLErr = err
		return err
	}
	// Docker Desktop may expose a host-side atomic rename to Redis a moment late.
	for attempt := 0; attempt < 20; attempt++ {
		a.loadACLErr = a.client.Do(ctx, "ACL", "LOAD").Err()
		if a.loadACLErr == nil || !strings.Contains(a.loadACLErr.Error(), "opening file '/acl/users.acl': No such file or directory") {
			return a.loadACLErr
		}
		time.Sleep(100 * time.Millisecond)
	}
	return a.loadACLErr
}
func (a *integrationAdminFixture) ListClients(ctx context.Context) ([]ClientIdentity, error) {
	rows, err := a.client.Do(ctx, "CLIENT", "LIST", "TYPE", "normal").Text()
	if err != nil {
		return nil, err
	}
	var clients []ClientIdentity
	for _, row := range strings.Split(rows, "\n") {
		fields := strings.Fields(row)
		values := map[string]string{}
		for _, field := range fields {
			pair := strings.SplitN(field, "=", 2)
			if len(pair) == 2 {
				values[pair[0]] = pair[1]
			}
		}
		if values["user"] != "" {
			clients = append(clients, ClientIdentity{ID: values["id"], Username: values["user"]})
		}
	}
	return clients, nil
}
func (a *integrationAdminFixture) ProbeAuth(ctx context.Context, username, password string) error {
	opts := *a.client.Options()
	opts.Username = username
	opts.Password = password // pragma: allowlist secret -- disposable ACL fixture
	client := redis.NewClient(&opts)
	pingErr := client.Ping(ctx).Err()
	return errors.Join(pingErr, client.Close())
}
func (a *integrationAdminFixture) UserExists(ctx context.Context, username string) (bool, error) {
	value, err := a.client.Do(ctx, "ACL", "GETUSER", username).Result()
	if err != nil {
		if err == redis.Nil || strings.Contains(err.Error(), "null") {
			return false, nil
		}
		return false, err
	}
	return value != nil, nil
}
func (a *integrationAdminFixture) KillUser(ctx context.Context, username string) error {
	return a.client.Do(ctx, "CLIENT", "KILL", "USER", username).Err()
}
func (a *integrationAdminFixture) ExecuteOperator(context.Context, []string) (string, error) {
	return "", fmt.Errorf("unused")
}

// Redis 7 uses the generic script ACL refusal; Redis 8 names the denied
// command and user. Require a Redis server error and the complete known prefix
// plus script-location suffix so transport or unrelated script errors fail.
func requireBudgetTimeACLRefusal(t *testing.T, err error, user string) {
	t.Helper()
	var serverError redis.Error
	require.ErrorAs(t, err, &serverError)
	message := serverError.Error()
	redis7 := strings.HasPrefix(message, "ERR The user executing the script can't run this command or subcommand script: ")
	redis8 := strings.HasPrefix(message, "ERR ACL failure in script: User "+user+" has no permissions to run the 'time' command script: ")
	require.True(t, (redis7 || redis8) && strings.Contains(message, "@user_script:"), "expected supported Redis script TIME ACL refusal, got %q", message)
}

// Only the disposable fixture's ACL file is changed. The production admin has
// ACL LOAD, not ACL SETUSER; restore the original grants before positive tests.
func assertBudgetScriptRequiresTimeGrant(ctx context.Context, t *testing.T, aclPath string, admin *integrationAdminFixture, cp *redis.Client, script, key string) {
	t.Helper()
	original, err := os.ReadFile(aclPath) // #nosec G304 -- sole caller passes the fixed users.acl path in its t.TempDir fixture.
	require.NoError(t, err)
	writeACL := func(contents []byte) {
		t.Helper()
		require.NoError(t, os.Chmod(aclPath, 0o600))
		require.NoError(t, os.WriteFile(aclPath, contents, 0o400)) // #nosec G703 -- sole caller owns this t.TempDir ACL file; contents only remove/restore its TIME grant.
		require.NoError(t, os.Chmod(aclPath, 0o400))
		require.NoError(t, admin.LoadACL(ctx, aclPath))
	}
	lines := strings.Split(string(original), "\n")
	removed := 0
	for i, line := range lines {
		if !strings.HasPrefix(line, "user cv_cp_12 ") {
			continue
		}
		tokens := strings.Fields(line)
		kept := make([]string, 0, len(tokens))
		for _, token := range tokens {
			if token == "+time" {
				removed++
				continue
			}
			kept = append(kept, token)
		}
		lines[i] = strings.Join(kept, " ")
	}
	require.Equal(t, 1, removed, "fixture must remove exactly the intended grant")
	require.NoError(t, cp.Del(ctx, key).Err())
	defer writeACL(original)
	writeACL([]byte(strings.Join(lines, "\n")))
	_, err = cp.Eval(ctx, script, []string{key}, "1", "5000000", "8").Result()
	requireBudgetTimeACLRefusal(t, err, "cv_cp_12")
	exists, err := cp.Exists(ctx, key).Result()
	require.NoError(t, err)
	require.Zero(t, exists, "missing TIME must refuse before creating account debt")
}

func integrationPolicy(passwords map[string]string) Policy {
	policy := policyTestValue()
	for service, generations := range map[string][]string{"control-plane": {"cv_cp_11", "cv_cp_12"}, "media-plane": {"cv_mp_11", "cv_mp_12"}} {
		value := policy.Services[service]
		for i, user := range generations {
			hash := sha256.Sum256([]byte(passwords[user]))
			value.Generations[i].PasswordHash = "#" + hex.EncodeToString(hash[:])
		}
		policy.Services[service] = value
	}
	return policy
}

func appendACLDiagnosticUserOnDisk(path string) error {
	contents, err := os.ReadFile(path) // #nosec G304 -- called only by the integration admin with its controller's fixed t.TempDir ACL path.
	if err != nil {
		return err
	}
	text := string(contents)
	addChecker := !strings.Contains(text, "user acl_checker ")
	addFlat := !strings.Contains(text, "user cv_mp_flat ")
	if !addChecker && !addFlat {
		return nil
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	if addChecker {
		text += "user acl_checker on resetpass >acl-checker-fixture resetkeys resetchannels -@all +acl|dryrun\n"
	}
	if addFlat {
		found := false
		for _, line := range strings.Split(text, "\n") {
			if !strings.HasPrefix(line, "user cv_mp_12 ") {
				continue
			}
			if selectorAt := strings.Index(line, " (+eval "); selectorAt >= 0 {
				line = line[:selectorAt]
			}
			text += strings.Replace(line, "user cv_mp_12 ", "user cv_mp_flat ", 1) + "\n"
			found = true
			break
		}
		if !found {
			return fmt.Errorf("cannot clone candidate media user for flat-ACL falsification")
		}
	}
	// #nosec G703 -- the integration controller supplies only its fixed t.TempDir ACL path; no request or Redis response supplies a path.
	if err := os.WriteFile(path, []byte(text), 0o400); err != nil {
		return err
	}
	return nil
}

func diagnosticClient(admin *redis.Client) *redis.Client {
	opts := *admin.Options()
	opts.Username = "acl_checker"
	opts.Password = "acl-checker-fixture" // pragma: allowlist secret -- disposable ACL fixture
	return redis.NewClient(&opts)
}

func integrationRedisClient(t *testing.T, addr, username, password string) *redis.Client {
	t.Helper()
	opts := &redis.Options{Addr: addr}
	// Each test uses a fresh Docker Redis, so DB 0 is isolated and avoids granting
	// SELECT to the ACL fixture users.
	opts.DB = 0
	opts.Username = username
	opts.Password = password // pragma: allowlist secret -- disposable ACL fixture
	opts.ReadTimeout = time.Second
	opts.DialTimeout = time.Second
	client := redis.NewClient(opts)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func eventually(timeout, interval time.Duration, check func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return true
		}
		time.Sleep(interval)
	}
	return check()
}

func dockerPortNumber(t *testing.T, output string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimSpace(output))
	require.NoError(t, err)
	return port
}

func integrationDryRunArgs(command, key string) []any {
	key = strings.TrimSuffix(key, "*") + "acl-fixture"
	switch command {
	case "ACL|WHOAMI":
		return []any{"ACL", "WHOAMI"}
	case "DECRBY", "INCRBY":
		return []any{command, "stepup:softlock:acl-fixture", "1"}
	case "DEL", "UNLINK":
		return []any{command, key}
	case "EVAL", "EVALSHA":
		return []any{command, "return 1", "0"}
	case "EXEC":
		return []any{command}
	case "EXISTS":
		return []any{command, key}
	case "EXPIRE":
		return []any{command, "auth_failures:ip:acl-fixture", "30"}
	case "GET", "GETDEL", "HGETALL", "HLEN", "PTTL", "TTL", "TYPE":
		return []any{command, key}
	case "HGET":
		return []any{command, "attach_sess:acl-fixture", "status"}
	case "HDEL":
		return []any{command, "token_theft_pending_revocations:fallback:v2", "acl-fixture"}
	case "HSET":
		return []any{command, "attach_sess:acl-fixture", "status", "x"}
	case "INCR":
		return []any{command, "login_attempts:acl-fixture"}
	case "MGET":
		return []any{command, key, key}
	case "MULTI", "PING", "UNWATCH":
		return []any{command}
	case "PEXPIRE":
		return []any{command, "voice:room_live:acl-fixture", "30000"}
	case "RPUSH":
		return []any{command, "attach_sess:acl-fixture", "x"}
	case "SADD", "SREM":
		return []any{command, "attestation:acl-fixture", "x"}
	case "SCARD", "SMEMBERS":
		return []any{command, "attestation:acl-fixture"}
	case "SCAN":
		return []any{command, "0", "MATCH", key}
	case "SET":
		return []any{command, key, "x"}
	case "SISMEMBER":
		return []any{command, "attestation:acl-fixture", "x"}
	case "WATCH":
		return []any{command, "recovery_code:acl-fixture"}
	case "WAITAOF":
		return []any{command, "1", "0", "1000"}
	case "ZADD":
		return []any{command, "session_revokes:acl-fixture", "1", "x"}
	case "ZCARD":
		return []any{command, "session_revokes:acl-fixture"}
	case "ZREMRANGEBYSCORE":
		return []any{command, "session_revokes:acl-fixture", "-inf", "0"}
	default:
		return []any{command}
	}
}

func integrationOperatorDryRunArgs(command string) []any {
	key := "voice:room_owner:01234567-89ab-cdef-0123-456789abcdef"
	switch command {
	case "DEL", "EXISTS", "GET":
		return []any{command, "media:active_colour"}
	case "EVAL":
		return []any{"EVAL", cleanupRoomScript, "2", key, "voice:room_live:01234567-89ab-cdef-0123-456789abcdef", "generation", "blue"}
	case "SADD", "SISMEMBER", "SREM":
		return []any{command, "media:colours:registered", "blue"}
	case "SCAN":
		return []any{"SCAN", "0", "MATCH", "voice:room_owner:*"}
	case "SET":
		return []any{"SET", "media:active_colour", "blue"}
	default:
		return []any{command}
	}
}

func readMediaLuaScript(t *testing.T, name string) string {
	t.Helper()
	source, err := os.ReadFile(filepath.Join("..", "..", "..", "media-plane", "src", "lib", "redis.ts"))
	require.NoError(t, err)
	pattern := regexp.MustCompile("(?s)const " + regexp.QuoteMeta(name) + "\\s*=\\s*(?:`([^`]*)`|\"([^\"]*)\")")
	matches := pattern.FindSubmatch(source)
	require.Len(t, matches, 3, "source must declare %s", name)
	if len(matches[1]) != 0 {
		return string(matches[1])
	}
	return string(matches[2])
}
