package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func testRecoveryIssue(userID, sendID, recordKey string) recoveryIssue {
	return recoveryIssue{
		RecordKey: recordKey,
		SlotKey:   recoverySlotKey(userID),
		SendsKey:  recoverySendsKey(userID),
		SendID:    sendID,
		Record:    []byte(`{"user_id":"` + userID + `","send_id":"` + sendID + `","addr":"owner@example.test"}`),
	}
}

func TestRecoveryEmailTokenShape(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		token, err := generateRecoveryEmailToken()
		require.NoError(t, err)
		require.Len(t, token, recoveryEmailTokenLen)
		raw, err := base64.RawURLEncoding.DecodeString(token)
		require.NoError(t, err)
		require.Len(t, raw, 32, "a recovery key carries 256 random bits")
		require.True(t, validRecoveryEmailToken(token))
		require.False(t, seen[token])
		seen[token] = true
	}
	require.False(t, validRecoveryEmailToken("123456"))
	require.False(t, validRecoveryEmailToken(strings.Repeat("!", recoveryEmailTokenLen)))
}

func TestRecoveryEmailTokenKeyHoldsOnlyAHash(t *testing.T) {
	token, err := generateRecoveryEmailToken()
	require.NoError(t, err)
	key := recoveryEmailTokenKey(token)
	require.True(t, strings.HasPrefix(key, recoveryTokenKeyPrefix))
	require.NotContains(t, key, token)
	require.Len(t, strings.TrimPrefix(key, recoveryTokenKeyPrefix), 64)
}

func TestValidRecoveryEmail(t *testing.T) {
	for addr, want := range map[string]bool{
		"a@b.c":       true,
		"slot:a@b.c":  false,
		"sends:a@b.c": false,
		"token:x":     false,
		"ab.c":        false,
		"":            false,
	} {
		require.Equal(t, want, validRecoveryEmail(addr), addr)
	}
}

func TestIssueRecoveryRecordTiers(t *testing.T) {
	rdb, mini := newRecoveryRedis(t)
	h := NewHandler(nil, rdb, logger.New("test"), "test", nil)
	ctx := context.Background()

	for i, want := range []time.Duration{recoveryFastTierTTL, recoveryFastTierTTL, recoveryFastTierTTL, recoverySlowTierTTL, recoverySlowTierTTL} {
		in := testRecoveryIssue("u1", "s"+string(rune('a'+i)), recoveryTokenKeyPrefix+string(rune('a'+i)))
		ttl, held, err := h.issueRecoveryRecord(ctx, in)
		require.NoError(t, err)
		require.False(t, held)
		require.Equal(t, want, ttl, "send %d", i+1)
		require.Equal(t, want, mini.TTL(in.RecordKey), "record TTL equals the cooldown")
		require.Equal(t, want, mini.TTL(in.SlotKey))
		require.Equal(t, recoverySendsWindow-time.Duration(i)*time.Minute, mini.TTL(in.SendsKey), "the window runs from the first send")
		mini.FastForward(time.Minute)
		mini.Del(in.SlotKey)
	}
}

func TestIssueRecoveryRecordHeldSendsNothing(t *testing.T) {
	rdb, mini := newRecoveryRedis(t)
	h := NewHandler(nil, rdb, logger.New("test"), "test", nil)
	ctx := context.Background()

	_, held, err := h.issueRecoveryRecord(ctx, testRecoveryIssue("u1", "s1", recoveryTokenKeyPrefix+"one"))
	require.NoError(t, err)
	require.False(t, held)

	second := testRecoveryIssue("u1", "s2", recoveryRedisKey("user@example.test"))
	_, held, err = h.issueRecoveryRecord(ctx, second)
	require.NoError(t, err)
	require.True(t, held, "a live recovery email in either format holds the slot")
	require.False(t, mini.Exists(second.RecordKey))
	count, err := mini.Get(second.SendsKey)
	require.NoError(t, err)
	require.Equal(t, "1", count)
	slot, err := mini.Get(second.SlotKey)
	require.NoError(t, err)
	require.Equal(t, "s1", slot)
}

// A slot held by a 6-digit code must not block a recovery key: the code may be
// unusable (its address budget spent, or a client that cannot enter it), and a
// key cannot be guessed. A key replaces the code once; nothing replaces a key,
// and a code never replaces anything (#344 review).
func TestIssueRecoveryRecordTokenReplacesCodeHeldSlot(t *testing.T) {
	rdb, mini := newRecoveryRedis(t)
	h := NewHandler(nil, rdb, logger.New("test"), "test", nil)
	ctx := context.Background()
	codeKey := recoveryRedisKey("user@example.test")

	_, held, err := h.issueRecoveryRecord(ctx, testRecoveryIssue("u1", recoveryCodeSendPrefix+"1", codeKey))
	require.NoError(t, err)
	require.False(t, held)

	code2 := testRecoveryIssue("u1", recoveryCodeSendPrefix+"2", codeKey)
	_, held, err = h.issueRecoveryRecord(ctx, code2)
	require.NoError(t, err)
	require.True(t, held, "a code never replaces a live code")

	token := testRecoveryIssue("u1", "t-1", recoveryTokenKeyPrefix+"one")
	token.CodeKey = codeKey
	ttl, held, err := h.issueRecoveryRecord(ctx, token)
	require.NoError(t, err)
	require.False(t, held, "a key replaces a slot a code holds")
	require.Equal(t, recoveryFastTierTTL, ttl)
	require.False(t, mini.Exists(codeKey), "the replaced code is no longer redeemable")
	require.True(t, mini.Exists(token.RecordKey))
	require.Equal(t, "t-1", mustGet(t, mini, token.SlotKey))
	require.Equal(t, "2", mustGet(t, mini, token.SendsKey), "the replacement counts as a send")

	again := testRecoveryIssue("u1", "t-2", recoveryTokenKeyPrefix+"two")
	again.CodeKey = codeKey
	_, held, err = h.issueRecoveryRecord(ctx, again)
	require.NoError(t, err)
	require.True(t, held, "nothing replaces a live key")
	_, held, err = h.issueRecoveryRecord(ctx, testRecoveryIssue("u1", recoveryCodeSendPrefix+"3", codeKey))
	require.NoError(t, err)
	require.True(t, held, "a code never replaces a key")
	require.Equal(t, "t-1", mustGet(t, mini, token.SlotKey))
}

// A code record written before slots existed has no slot. A code request keeps
// it (with its attempt count) and sends nothing; a key request deletes it.
func TestIssueRecoveryRecordPreUpgradeCodeWithoutSlot(t *testing.T) {
	rdb, mini := newRecoveryRedis(t)
	h := NewHandler(nil, rdb, logger.New("test"), "test", nil)
	ctx := context.Background()
	codeKey := recoveryRedisKey("user@example.test")
	const preUpgrade = `{"code_hash":"h","user_id":"u1","attempts":3}`
	require.NoError(t, mini.Set(codeKey, preUpgrade))

	_, held, err := h.issueRecoveryRecord(ctx, testRecoveryIssue("u1", recoveryCodeSendPrefix+"1", codeKey))
	require.NoError(t, err)
	require.True(t, held, "a code request leaves a live pre-upgrade code alone")
	require.Equal(t, preUpgrade, mustGet(t, mini, codeKey), "its attempt count is not reset")
	require.False(t, mini.Exists(recoverySendsKey("u1")), "a held request counts no send")

	token := testRecoveryIssue("u1", "t-1", recoveryTokenKeyPrefix+"one")
	token.CodeKey = codeKey
	_, held, err = h.issueRecoveryRecord(ctx, token)
	require.NoError(t, err)
	require.False(t, held)
	require.False(t, mini.Exists(codeKey), "a key request deletes the pre-upgrade code")
	require.Equal(t, "t-1", mustGet(t, mini, token.SlotKey))
}

// A code email whose slot a key took before the code was sent is not sent:
// the newest email the owner gets must hold a usable secret.
func TestRecoveryCodeDeliverySkippedAfterKeyReplacement(t *testing.T) {
	rdb, _ := newRecoveryRedis(t)
	h := NewHandler(nil, rdb, logger.New("test"), "test", nil)
	mail := &fakeRecoveryMailer{}
	h.recoveryMail = mail
	t.Cleanup(h.CloseRecoveryJobs)
	ctx := context.Background()
	codeKey := recoveryRedisKey("owner@example.test")

	code := testRecoveryIssue("u1", recoveryCodeSendPrefix+"1", codeKey)
	_, _, err := h.issueRecoveryRecord(ctx, code)
	require.NoError(t, err)
	token := testRecoveryIssue("u1", "t-1", recoveryTokenKeyPrefix+"one")
	token.CodeKey = codeKey
	_, _, err = h.issueRecoveryRecord(ctx, token)
	require.NoError(t, err)

	// Deliveries finish in reverse order: the key email first, then the code.
	h.deliverRecoveryCode(&recoveryDelivery{addr: "owner@example.test", secret: "k", format: recoveryFormatToken, minutes: 15, issue: token})
	h.deliverRecoveryCode(&recoveryDelivery{addr: "owner@example.test", secret: "123456", format: recoveryFormatCode, minutes: 15, issue: code})

	require.Len(t, mail.keys, 1)
	require.Empty(t, mail.codes, "a replaced code is never emailed")
}

// The begin path wires the replacement: an attacker who holds the slot with a
// code email cannot stop the owner's key email.
func TestRecoveryBeginTokenAfterCodeSendsKey(t *testing.T) {
	rdb, mini := newRecoveryRedis(t)
	h := NewHandler(nil, rdb, logger.New("test"), "test", nil)
	mail := &fakeRecoveryMailer{}
	h.recoveryMail = mail
	t.Cleanup(h.CloseRecoveryJobs)

	h.issueAndDeliverRecovery(t.Context(), recoveryJob{addr: "owner@example.test", format: recoveryFormatCode}, "user-1")
	h.issueAndDeliverRecovery(t.Context(), recoveryJob{addr: "owner@example.test", format: recoveryFormatToken}, "user-1")
	h.recoveryDeliveries.Wait()
	require.Len(t, mail.keys, 1, "the key email is sent although a code holds the slot")
	require.True(t, mini.Exists(recoveryEmailTokenKey(mail.keys[0])))
	require.False(t, mini.Exists(recoveryRedisKey("owner@example.test")))
}

// A Redis that refuses the call leaves no state. This covers a failure before
// the script runs; a failure inside it is handled by write order (record
// before slot), which miniredis cannot inject.
func TestIssueRecoveryRecordRedisFailureLeavesNoState(t *testing.T) {
	rdb, mini := newRecoveryRedis(t)
	h := NewHandler(nil, rdb, logger.New("test"), "test", nil)
	in := testRecoveryIssue("u1", "s1", recoveryTokenKeyPrefix+"one")

	mini.SetError("LOADING")
	_, _, err := h.issueRecoveryRecord(context.Background(), in)
	require.Error(t, err)
	mini.SetError("")
	require.False(t, mini.Exists(in.RecordKey))
	require.False(t, mini.Exists(in.SlotKey))
	require.False(t, mini.Exists(in.SendsKey))
}

func TestClearUnsentRecoveryRefundsOnlyOwnSend(t *testing.T) {
	rdb, mini := newRecoveryRedis(t)
	h := NewHandler(nil, rdb, logger.New("test"), "test", nil)
	ctx := context.Background()
	in := testRecoveryIssue("u1", "s1", recoveryTokenKeyPrefix+"one")
	_, _, err := h.issueRecoveryRecord(ctx, in)
	require.NoError(t, err)

	other := in
	other.SendID = "someone-else"
	require.NoError(t, h.clearUnsentRecovery(ctx, other))
	require.True(t, mini.Exists(in.RecordKey), "another send's cleanup changes nothing")
	require.True(t, mini.Exists(in.SlotKey))

	require.NoError(t, h.clearUnsentRecovery(ctx, in))
	require.False(t, mini.Exists(in.RecordKey))
	require.False(t, mini.Exists(in.SlotKey))
	count, err := mini.Get(in.SendsKey)
	require.NoError(t, err)
	require.Equal(t, "0", count, "an email never sent does not count toward the tier")
	require.Positive(t, mini.TTL(in.SendsKey), "the refund keeps the window")
}

func TestReleaseRecoverySlotOnlyOwnSend(t *testing.T) {
	rdb, mini := newRecoveryRedis(t)
	h := NewHandler(nil, rdb, logger.New("test"), "test", nil)
	ctx := context.Background()
	require.NoError(t, mini.Set(recoverySlotKey("u1"), "s2"))

	h.releaseRecoverySlot(ctx, "u1", "s1")
	require.True(t, mini.Exists(recoverySlotKey("u1")), "a stale redeem must not release a newer send's slot")
	h.releaseRecoverySlot(ctx, "u1", "s2")
	require.False(t, mini.Exists(recoverySlotKey("u1")))
}

type fakeRecoveryMailer struct {
	mu    sync.Mutex
	keys  []string
	codes []string
	mins  []int
	err   error
}

func (f *fakeRecoveryMailer) SendRecoveryCode(_, code string, minutes int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.codes = append(f.codes, code)
	f.mins = append(f.mins, minutes)
	return f.err
}

func (f *fakeRecoveryMailer) SendRecoveryKey(_, key string, minutes int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, key)
	f.mins = append(f.mins, minutes)
	return f.err
}

func (f *fakeRecoveryMailer) sent() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.keys) + len(f.codes)
}

func beginRecovery(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/recovery/begin", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.RecoveryBegin(c)
	return response
}

func TestRecoveryBeginFormatEcho(t *testing.T) {
	cases := []struct {
		name, body string
		legacyOff  bool
		status     int
		format     string
		queued     bool
	}{
		{name: "no format is the legacy code", body: `{"email":"a@b.test"}`, status: http.StatusOK, format: "code", queued: true},
		{name: "explicit code", body: `{"email":"a@b.test","format":"code"}`, status: http.StatusOK, format: "code", queued: true},
		{name: "token", body: `{"email":"a@b.test","format":"token"}`, status: http.StatusOK, format: "token", queued: true},
		{name: "legacy off issues a token", body: `{"email":"a@b.test"}`, legacyOff: true, status: http.StatusOK, format: "token", queued: true},
		{name: "unknown format", body: `{"email":"a@b.test","format":"link"}`, status: http.StatusBadRequest},
		{name: "crafted key name", body: `{"email":"slot:a@b.test","format":"token"}`, status: http.StatusBadRequest},
		{name: "no at sign", body: `{"email":"ab.test"}`, status: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler(nil, nil, logger.New("test"), "test", nil)
			h.SetRecoveryLegacyCode(!tc.legacyOff)
			jobs := make(chan recoveryJob, 1)
			h.recoveryProcess = func(job recoveryJob) { jobs <- job }
			response := beginRecovery(t, h, tc.body)
			h.CloseRecoveryJobs()

			require.Equal(t, tc.status, response.Code)
			select {
			case job := <-jobs:
				require.True(t, tc.queued, "a refused request reached the queue")
				require.Equal(t, recoveryFormat(tc.format), job.format)
				require.JSONEq(t, `{"message":"If an account exists with that email, a recovery email has been sent","format":"`+tc.format+`"}`, response.Body.String())
			default:
				require.False(t, tc.queued, "an accepted request was not queued")
			}
		})
	}
}

func TestRecoveryBeginTokenSendsKeyEmail(t *testing.T) {
	rdb, mini := newRecoveryRedis(t)
	h := NewHandler(nil, rdb, logger.New("test"), "test", nil)
	mail := &fakeRecoveryMailer{}
	h.recoveryMail = mail
	t.Cleanup(h.CloseRecoveryJobs)

	h.issueAndDeliverRecovery(t.Context(), recoveryJob{addr: "owner@example.test", format: recoveryFormatToken}, "user-1")
	h.recoveryDeliveries.Wait()

	require.Len(t, mail.keys, 1)
	key := mail.keys[0]
	require.True(t, validRecoveryEmailToken(key))
	require.Equal(t, []int{15}, mail.mins)
	stored, err := mini.Get(recoveryEmailTokenKey(key))
	require.NoError(t, err)
	require.JSONEq(t, `{"user_id":"user-1","send_id":"`+mustGet(t, mini, recoverySlotKey("user-1"))+`","addr":"owner@example.test"}`, stored)
	for _, k := range mini.Keys() {
		v, _ := mini.Get(k)
		require.NotContains(t, k+v, key, "the raw key must not be stored")
	}
}

func mustGet(t *testing.T, mini *miniredis.Miniredis, key string) string {
	t.Helper()
	v, err := mini.Get(key)
	require.NoError(t, err)
	return v
}

func TestRecoveryBeginAlternatingFormatsShareSlot(t *testing.T) {
	rdb, _ := newRecoveryRedis(t)
	h := NewHandler(nil, rdb, logger.New("test"), "test", nil)
	mail := &fakeRecoveryMailer{}
	h.recoveryMail = mail
	t.Cleanup(h.CloseRecoveryJobs)

	// Let the code go out first; a code a key replaces before it is sent is
	// never sent (TestRecoveryCodeDeliverySkippedAfterKeyReplacement).
	h.issueAndDeliverRecovery(t.Context(), recoveryJob{addr: "owner@example.test", format: recoveryFormatCode}, "user-1")
	h.recoveryDeliveries.Wait()
	for _, f := range []recoveryFormat{recoveryFormatToken, recoveryFormatCode, recoveryFormatToken} {
		h.issueAndDeliverRecovery(t.Context(), recoveryJob{addr: "owner@example.test", format: f}, "user-1")
	}
	// A second address spelling for the same account shares the limit too.
	h.issueAndDeliverRecovery(t.Context(), recoveryJob{addr: "Owner@Example.test", format: recoveryFormatToken}, "user-1")
	h.recoveryDeliveries.Wait()
	// The code, then the key that replaces it; every later request is held.
	require.Equal(t, 2, mail.sent(), "at most a code and one key per cooldown, whatever the format or spelling")
}

func TestRecoveryBeginDeliveryFailureRefunds(t *testing.T) {
	rdb, mini := newRecoveryRedis(t)
	h := NewHandler(nil, rdb, logger.New("test"), "test", nil)
	mail := &fakeRecoveryMailer{err: errors.New("smtp down")}
	h.recoveryMail = mail
	t.Cleanup(h.CloseRecoveryJobs)

	h.issueAndDeliverRecovery(t.Context(), recoveryJob{addr: "owner@example.test", format: recoveryFormatToken}, "user-1")
	h.recoveryDeliveries.Wait()
	require.False(t, mini.Exists(recoveryEmailTokenKey(mail.keys[0])))
	require.False(t, mini.Exists(recoverySlotKey("user-1")), "a failed send must not hold the slot")
	require.Equal(t, "0", mustGet(t, mini, recoverySendsKey("user-1")))
}

func TestRecoveryBeginRedisDownLeavesNoState(t *testing.T) {
	rdb, mini := newRecoveryRedis(t)
	var logs bytes.Buffer
	h := NewHandler(nil, rdb, logger.NewWithWriter(&logs), "test", nil)
	mail := &fakeRecoveryMailer{}
	h.recoveryMail = mail
	t.Cleanup(h.CloseRecoveryJobs)

	mini.SetError("LOADING")
	h.issueAndDeliverRecovery(t.Context(), recoveryJob{addr: "owner@example.test", format: recoveryFormatToken}, "user-1")
	mini.SetError("")
	h.recoveryDeliveries.Wait()
	require.Zero(t, mail.sent())
	require.Empty(t, mini.Keys())
	require.Contains(t, logs.String(), "failure_class=recovery_code_store")
	require.NotContains(t, logs.String(), "owner@example.test")
}

func verifyRecovery(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/recovery/verify-code", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.RecoveryVerifyCode(c)
	return response
}

func TestParseRecoveryVerifyRequest(t *testing.T) {
	token, err := generateRecoveryEmailToken()
	require.NoError(t, err)
	cases := []struct {
		name, body string
		legacyOff  bool
		ok         bool
		wantToken  string
		wantCode   string
		errText    string
	}{
		{name: "token", body: `{"token":"` + token + `"}`, ok: true, wantToken: token},
		{name: "token with pasted whitespace", body: `{"token":"  ` + token + `\n"}`, ok: true, wantToken: token},
		{name: "token split across lines", body: `{"token":"` + token[:20] + `\r\n ` + token[20:] + `"}`, ok: true, wantToken: token},
		{name: "token ignores email", body: `{"email":"x","token":"` + token + `"}`, ok: true, wantToken: token},
		{name: "code", body: `{"email":"a@b.test","code":"123-456"}`, ok: true, wantCode: "123456"},
		{name: "both", body: `{"email":"a@b.test","code":"123456","token":"` + token + `"}`, errText: "exactly one"},
		{name: "neither", body: `{"email":"a@b.test"}`, errText: "exactly one"},
		{name: "null token", body: `{"token":null}`, errText: "exactly one"},
		{name: "numeric token", body: `{"token":7}`},
		{name: "short token", body: `{"token":"abc"}`, errText: "Invalid recovery key"},
		{name: "crafted email on code path", body: `{"email":"slot:a@b.test","code":"123456"}`, errText: errValidEmailRequired},
		{name: "code path without email", body: `{"code":"123456"}`, errText: errValidEmailRequired},
		{name: "legacy off refuses code", body: `{"email":"a@b.test","code":"123456"}`, legacyOff: true, errText: "Update the app"},
		{name: "legacy off accepts token", body: `{"token":"` + token + `"}`, legacyOff: true, ok: true, wantToken: token},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(response)
			c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			req, ok := parseRecoveryVerifyRequest(c, !tc.legacyOff)
			require.Equal(t, tc.ok, ok)
			if !tc.ok {
				require.Equal(t, http.StatusBadRequest, response.Code)
				require.Contains(t, response.Body.String(), tc.errText)
				return
			}
			require.Equal(t, tc.wantToken, req.Token)
			require.Equal(t, tc.wantCode, req.Code)
		})
	}
}

func TestConsumeRecoveryEmailTokenSingleUse(t *testing.T) {
	rdb, mini := newRecoveryRedis(t)
	h := NewHandler(nil, rdb, logger.New("test"), "test", nil)
	ctx := context.Background()
	token, err := generateRecoveryEmailToken()
	require.NoError(t, err)
	in := testRecoveryIssue("user-1", "s1", recoveryEmailTokenKey(token))
	_, _, err = h.issueRecoveryRecord(ctx, in)
	require.NoError(t, err)

	record, err := h.consumeRecoveryEmailToken(ctx, token)
	require.NoError(t, err)
	require.Equal(t, "user-1", record.UserID)
	require.False(t, mini.Exists(in.SlotKey), "a redeemed key frees the account's send slot")

	_, err = h.consumeRecoveryEmailToken(ctx, token)
	require.ErrorIs(t, err, redis.Nil, "a key works once")
}

func TestConsumeRecoveryEmailTokenConcurrent(t *testing.T) {
	rdb, _ := newRecoveryRedis(t)
	h := NewHandler(nil, rdb, logger.New("test"), "test", nil)
	token, err := generateRecoveryEmailToken()
	require.NoError(t, err)
	_, _, err = h.issueRecoveryRecord(context.Background(), testRecoveryIssue("user-1", "s1", recoveryEmailTokenKey(token)))
	require.NoError(t, err)

	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.consumeRecoveryEmailToken(context.Background(), token); err == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int32(1), wins.Load())
}

func TestTokenFailuresWriteNoBudget(t *testing.T) {
	rdb, mini := newRecoveryRedis(t)
	h := NewHandler(nil, rdb, logger.New("test"), "test", nil)
	owner, err := generateRecoveryEmailToken()
	require.NoError(t, err)
	in := testRecoveryIssue("user-1", "s1", recoveryEmailTokenKey(owner))
	_, _, err = h.issueRecoveryRecord(context.Background(), in)
	require.NoError(t, err)
	before := map[string]string{}
	for _, k := range mini.Keys() {
		before[k], _ = mini.Get(k)
	}

	var body string
	for range 50 {
		wrong, err := generateRecoveryEmailToken()
		require.NoError(t, err)
		body = `{"email":"owner@example.test","token":"` + wrong + `"}`
		response := verifyRecovery(t, h, body)
		require.Equal(t, http.StatusUnauthorized, response.Code)
		require.JSONEq(t, `{"error":"Invalid or expired recovery code"}`, response.Body.String())
	}
	for _, k := range mini.Keys() {
		require.NotContains(t, k, recoveryFailureKeyPrefix, "a wrong key spends no account budget")
		if strings.HasPrefix(k, "recovery_code:") {
			v, _ := mini.Get(k)
			require.Equal(t, before[k], v, "a wrong key changes no recovery state")
		}
	}
	record, err := h.consumeRecoveryEmailToken(context.Background(), owner)
	require.NoError(t, err, "the owner's key still works")
	require.Equal(t, "user-1", record.UserID)
}
