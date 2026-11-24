package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const (
	corpusRelativePath = "docs/design/nonmedia-e2ee-v1-vectors.json"
	adrRelativePath    = "docs/adr/0051-nonmedia-e2ee-context-and-credential-domains.md"
)

var (
	hex384           = regexp.MustCompile(`^[0-9a-f]{96}$`)
	pin384           = regexp.MustCompile(`(?m)^\*\*Corpus SHA-384:\*\* ` + "`" + `([0-9a-f]{96})` + "`" + `$`)
	contextTagFields = map[string][]string{
		"message-v1":             {"realm", "scope", "scope_id", "message_id", "sender_id", "key_epoch", "content_kind"},
		"blob-v1":                {"realm", "account_id", "blob_purpose", "schema_version"},
		"account-ek-wrap-v1":     {"realm", "account_id", "key_purpose", "account_key_generation", "credential_generation"},
		"recovery-wrap-v1":       {"realm", "account_id", "key_purpose", "recovery_purpose", "recovery_key_generation", "account_key_generation"},
		"recovery-prefs-wrap-v1": {"realm", "account_id", "key_purpose", "recovery_purpose", "recovery_key_generation", "credential_generation"},
		"attachment-v4":          {"realm", "scope", "scope_id", "file_id", "uploader_id", "key_epoch", "file_nonce_b64", "flags", "chunk_index", "total_chunks", "chunk_size"},
		"channel-key-capsule-v1": {"realm", "scope", "scope_id", "key_epoch", "recipient_id", "recipient_key_generation", "distributor_id", "distributor_root_digest_b64"},
	}
)

type corpus struct {
	Schema          string            `json:"schema"`
	Version         int               `json:"version"`
	GeneratorSHA384 string            `json:"generator_sha384"`
	VectorsSHA384   string            `json:"vectors_sha384"`
	Vectors         []json.RawMessage `json:"vectors"`
}

type vector struct {
	Name       string          `json:"name"`
	Suite      string          `json:"suite"`
	Operation  string          `json:"operation"`
	Cite       string          `json:"cite"`
	Input      json.RawMessage `json:"input"`
	NegativeOf string          `json:"negative_of"`
	Mutation   json.RawMessage `json:"mutation"`
	Expect     json.RawMessage `json:"expect"`
}

func TestCorpusCanonicalBytesAndPins(t *testing.T) {
	root := repositoryRoot(t)
	corpusBytes := readFile(t, filepath.Join(root, corpusRelativePath))
	if len(corpusBytes) > 1<<20 {
		t.Fatalf("corpus exceeds 1 MiB: %d bytes", len(corpusBytes))
	}
	if !bytes.HasSuffix(corpusBytes, []byte("\n")) || bytes.Count(corpusBytes, []byte("\n")) == 0 {
		t.Fatal("corpus must have exactly one final LF-delimited header/body boundary")
	}
	if bytes.Contains(corpusBytes, []byte("\r")) || bytes.HasPrefix(corpusBytes, []byte{0xef, 0xbb, 0xbf}) {
		t.Fatal("corpus must be ASCII with LF line endings and no BOM")
	}
	for _, b := range corpusBytes {
		if b > 0x7f {
			t.Fatal("corpus contains non-ASCII byte")
		}
	}

	var c corpus
	decodeCorpus(t, corpusBytes, &c)
	if c.Schema != "concord-nonmedia-e2ee-vectors" || c.Version != 1 {
		t.Fatalf("unexpected corpus identity: schema=%q version=%d", c.Schema, c.Version)
	}
	if !hex384.MatchString(c.GeneratorSHA384) || !hex384.MatchString(c.VectorsSHA384) {
		t.Fatal("corpus pins must be lowercase SHA-384 hex")
	}
	if got := sha384Hex(readFile(t, filepath.Join(root, "services/control-plane/cmd/nonmedia-vectors/gen.go"))); got != c.GeneratorSHA384 {
		t.Fatalf("generator SHA-384 mismatch: got %s want %s", got, c.GeneratorSHA384)
	}
	bodyStart := bytes.IndexByte(corpusBytes, '\n') + 1
	if got := sha384Hex(corpusBytes[bodyStart:]); got != c.VectorsSHA384 {
		t.Fatalf("body SHA-384 mismatch: got %s want %s", got, c.VectorsSHA384)
	}
	adr := readFile(t, filepath.Join(root, adrRelativePath))
	pin := pin384.FindSubmatch(adr)
	if len(pin) != 2 {
		t.Fatal("ADR must pin the complete corpus as '**Corpus SHA-384:** `<96 lowercase hex>`'")
	}
	if got := sha384Hex(corpusBytes); got != string(pin[1]) {
		t.Fatalf("ADR whole-file SHA-384 mismatch: got %s want %s", got, pin[1])
	}
}

func TestGeneratorCheckModeMatchesCommittedCorpus(t *testing.T) {
	root := repositoryRoot(t)
	source := readFile(t, filepath.Join(root, "services/control-plane/cmd/nonmedia-vectors/gen.go"))
	generated := generate(source)
	if !bytes.Equal(generated, readFile(t, filepath.Join(root, corpusRelativePath))) {
		t.Fatal("deterministic generator differs from the committed corpus")
	}
	previous := os.Args
	os.Args = []string{"nonmedia-vectors", "--check"}
	t.Cleanup(func() { os.Args = previous })
	if err := run(); err != nil {
		t.Fatalf("generator --check: %v", err)
	}
	os.Args = []string{"nonmedia-vectors", "--unexpected"}
	if err := run(); err == nil {
		t.Fatal("unknown generator mode unexpectedly accepted")
	}
}

func TestCorpusHasClosedObjectShapesAndUniqueNames(t *testing.T) {
	corpusBytes := readFile(t, filepath.Join(repositoryRoot(t), corpusRelativePath))
	assertNoDuplicateObjectKeys(t, corpusBytes)
	assertCanonicalTopOrder(t, corpusBytes)

	var c corpus
	decodeCorpus(t, corpusBytes, &c)
	names := make(map[string]vector, len(c.Vectors))
	for _, raw := range c.Vectors {
		var v vector
		decodeCorpus(t, raw, &v)
		if !regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,127}$`).MatchString(v.Name) {
			t.Fatalf("invalid vector name %q", v.Name)
		}
		if _, exists := names[v.Name]; exists {
			t.Fatalf("duplicate vector name %q", v.Name)
		}
		names[v.Name] = v
		assertVectorOrder(t, raw, v)
		assertClosedOperationShape(t, v)
	}
	if len(names) != len(c.Vectors) {
		t.Fatal("corpus has duplicate names")
	}
	for _, v := range names {
		assertVectorMutation(t, v, names)
	}
}

func TestCorpusExhaustivelyPinsLegacyContinuationPolicy(t *testing.T) {
	vectors := policyVectors(t, "legacy-continuation")
	for _, device := range []string{"existing", "fresh"} {
		for _, protocol := range []string{"legacy", "split"} {
			for _, proof := range []string{"verified", "absent", "invalid"} {
				for _, realm := range []bool{true, false} {
					for _, account := range []bool{true, false} {
						for _, freshness := range []string{"current", "stale"} {
							for _, notExpired := range []bool{true, false} {
								for _, continuation := range []bool{true, false} {
									name := fmt.Sprintf("policy/legacy-continuation/matrix/%s/%s/%s/%s/%s/%s/%s/%s", device, protocol, proof, boolSlot(realm), boolSlot(account), freshness, boolSlot(notExpired), boolSlot(continuation))
									v, ok := vectors[name]
									if !ok {
										t.Fatalf("missing required legacy policy vector %q", name)
									}
									input := object(t, v.Input)
									mustEqualJSON(t, input, "device_state", device)
									mustEqualJSON(t, input, "account_protocol", protocol)
									mustEqualJSON(t, input, "authority_proof", proof)
									mustEqualJSON(t, input, "realm_match", realm)
									mustEqualJSON(t, input, "account_match", account)
									mustEqualJSON(t, input, "freshness", freshness)
									mustEqualJSON(t, input, "not_expired", notExpired)
									mustEqualJSON(t, input, "control_plane_continuation", continuation)
									decision, reason := legacyDecision(protocol, proof, realm, account, freshness, notExpired, continuation)
									assertPolicyExpect(t, v.Expect, decision, reason)
								}
							}
						}
					}
				}
			}
		}
	}
}

func TestCorpusContainsClosedRequiredCaseManifest(t *testing.T) {
	var c corpus
	decodeCorpus(t, readFile(t, filepath.Join(repositoryRoot(t), corpusRelativePath)), &c)
	byName := make(map[string]vector, len(c.Vectors))
	for _, raw := range c.Vectors {
		var v vector
		decodeCorpus(t, raw, &v)
		byName[v.Name] = v
	}

	tags := contextTagFields
	for tag, fields := range tags {
		requireNamedVector(t, byName, "context/encode/"+tag+"/base", "context", "encode")
		requireNamedVector(t, byName, "context/parse/"+tag+"/base", "context", "parse")
		if tag == "channel-key-capsule-v1" {
			requireNamedVector(t, byName, "rsa-label/derive/"+tag+"/base", "rsa-label", "derive")
			requireNamedVector(t, byName, "rsa-label/verify/"+tag+"/base", "rsa-label", "verify")
			for _, field := range fields {
				requireNamedVector(t, byName, "rsa-label/verify/"+tag+"/alter-"+field, "rsa-label", "verify")
			}
			continue
		}
		requireNamedVector(t, byName, "aead/encrypt/"+tag+"/base", "aead", "encrypt")
		requireNamedVector(t, byName, "aead/decrypt/"+tag+"/base", "aead", "decrypt")
		for _, field := range fields {
			requireNamedVector(t, byName, "aead/decrypt/"+tag+"/alter-"+field, "aead", "decrypt")
		}
		for _, length := range []string{"short-key", "long-key", "short-nonce", "long-nonce"} {
			requireNamedVector(t, byName, "aead/encrypt/"+tag+"/"+length, "aead", "encrypt")
		}
		for _, length := range []string{"short-key", "long-key", "short-nonce", "long-nonce", "wrong-format"} {
			requireNamedVector(t, byName, "aead/decrypt/"+tag+"/"+length, "aead", "decrypt")
		}
	}

	for _, purpose := range []string{"preferences", "saved_gifs", "friend_organization", "presence_overrides"} {
		requireNamedVector(t, byName, "context/encode/blob-v1/"+purpose, "context", "encode")
	}
	for _, tag := range []string{"account-ek-wrap-v1", "recovery-wrap-v1", "recovery-prefs-wrap-v1"} {
		requireNamedVector(t, byName, "context/encode/"+tag+"/wrong-key-purpose", "context", "encode")
	}
	for _, required := range []struct{ name, suite, operation string }{
		{"credential/password-scalars/invalid-surrogate", "credential", "password-scalars"},
		{"credential/derive/max-generation", "credential", "derive"},
		{"credential/descriptor/weak-parameters", "credential", "descriptor"},
		{"credential/descriptor/wrong-realm", "credential", "descriptor"},
		{"aead/decrypt/attachment-v4/tamper-header", "aead", "decrypt"},
		{"aead/decrypt/attachment-v4/tamper-flags", "aead", "decrypt"},
		{"aead/decrypt/attachment-v4/tamper-chunk-size", "aead", "decrypt"},
		{"aead/decrypt/attachment-v4/tamper-total-chunks", "aead", "decrypt"},
		{"aead/decrypt/attachment-v4/tamper-version", "aead", "decrypt"},
		{"aead/decrypt/attachment-v4/tamper-magic", "aead", "decrypt"},
		{"credential/derive/short-subject", "credential", "derive"},
		{"credential/derive/short-salt", "credential", "derive"},
		{"credential/derive/invalid-password-utf8", "credential", "derive"},
		{"credential/descriptor/noncanonical-whitespace", "credential", "descriptor"},
		{"credential/descriptor/invalid-subject-base64", "credential", "descriptor"},
		{"credential/descriptor/wrong-discriminator", "credential", "descriptor"},
		{"credential/descriptor/zero-generation", "credential", "descriptor"},
		{"credential/descriptor/trailing-field", "credential", "descriptor"},
		{"rsa-label/derive/channel-key-capsule-v1/invalid-recipient-id", "rsa-label", "derive"},
		{"context/encode/message-v1/unknown-tag", "context", "encode"},
		{"context/encode/message-v1/invalid-realm", "context", "encode"},
		{"context/parse/message-v1/unknown-tag", "context", "parse"},
		{"context/parse/message-v1/noncanonical-whitespace", "context", "parse"},
		{"context/parse/message-v1/invalid-realm", "context", "parse"},
		{"context/parse/account-ek-wrap-v1/wrong-key-purpose", "context", "parse"},
		{"context/parse/attachment-v4/invalid-flags", "context", "parse"},
		{"context/parse/attachment-v4/invalid-chunk-index", "context", "parse"},
		{"context/parse/attachment-v4/fractional-chunk-index", "context", "parse"},
		{"context/parse/attachment-v4/fractional-total-chunks", "context", "parse"},
	} {
		requireNamedVector(t, byName, required.name, required.suite, required.operation)
	}
	for _, class := range []string{"split", "legacy", "unknown"} {
		requireNamedVector(t, byName, "policy/descriptor-uniformity/"+class, "policy", "descriptor-uniformity")
	}
	for _, name := range []string{"eligible", "forged-split", "invalid-proof", "existing-device-no-proof", "stale-post-upgrade", "wrong-account", "wrong-realm", "expired-proof", "no-continuation", "fresh-device-no-proof", "fresh-device-invalid-proof"} {
		requireNamedVector(t, byName, "policy/legacy-continuation/"+name, "policy", "legacy-continuation")
	}
	for _, name := range []string{"fresh-authorization", "fresh-request-conflict", "already-split", "replay", "request-conflict", "mfa-required", "ambiguous-commit", "expired-authorization", "missing-authorization"} {
		requireNamedVector(t, byName, "policy/upgrade-outcome/"+name, "policy", "upgrade-outcome")
	}
	assertRequiredRejectCategories(t, byName, tags)
	assertRequiredCandidateMutationManifest(t, byName)
	assertNamedPolicyPredicates(t, byName)
}

func TestCorpusExhaustivelyPinsUpgradeOutcomePolicy(t *testing.T) {
	vectors := policyVectors(t, "upgrade-outcome")
	for _, prior := range []string{"legacy", "split"} {
		for _, authorization := range []string{"fresh", "consumed", "expired", "absent"} {
			for _, match := range []bool{true, false} {
				for _, mfa := range []bool{true, false} {
					for _, commit := range []string{"none", "committed", "ambiguous"} {
						name := fmt.Sprintf("policy/upgrade-outcome/matrix/%s/%s/%s/%s/%s", prior, authorization, boolSlot(match), boolSlot(mfa), commit)
						v, ok := vectors[name]
						if !ok {
							t.Fatalf("missing required upgrade policy vector %q", name)
						}
						input := object(t, v.Input)
						mustEqualJSON(t, input, "prior_state", prior)
						mustEqualJSON(t, input, "authorization_state", authorization)
						mustEqualJSON(t, input, "request_match", match)
						mustEqualJSON(t, input, "mfa_complete", mfa)
						mustEqualJSON(t, input, "commit_state", commit)
						decision, reason := upgradeDecision(prior, authorization, match, mfa, commit)
						assertPolicyExpect(t, v.Expect, decision, reason)
					}
				}
			}
		}
	}
}

// This deliberately recomputes positive cases without calling generator
// helpers, so a self-consistent generator cannot attest its own crypto bytes.
func TestCorpusPositiveVectorsMatchIndependentGoPrimitives(t *testing.T) {
	vectors := corpusVectorsByName(t)
	t.Run("credential derive", func(t *testing.T) {
		v := requireNamedVector(t, vectors, "credential/derive/base", "credential", "derive")
		input := object(t, v.Input)
		outputs := acceptedOutputs(t, v)
		password := mustBase64(t, input["password_utf8_b64"])
		subject := mustBase64(t, input["subject_b64"])
		salt := mustBase64(t, input["salt_b64"])
		generation, ok := input["generation"].(float64)
		if !ok || generation < 1 || generation != float64(uint64(generation)) {
			t.Fatalf("invalid generation %#v", input["generation"])
		}
		root := argon2.IDKey(password, salt, 3, 65536, 4, 32)
		var generationBytes [8]byte
		binary.BigEndian.PutUint64(generationBytes[:], uint64(generation))
		bind := append([]byte("concord-credential-bind-v1\x00"+stringValue(t, input["realm"])), subject...)
		bind = append(bind, generationBytes[:]...)
		prk := hmacSHA256(bind, root) // HKDF-Extract(salt=bind, IKM=root).
		assertEncodedOutput(t, outputs, "root_b64", root, base64.StdEncoding)
		assertEncodedOutput(t, outputs, "auth_b64", hkdfExpand32(prk, "concord-auth-secret-v1\x00"), base64.RawURLEncoding)
		assertEncodedOutput(t, outputs, "wrap_b64", hkdfExpand32(prk, "concord-wrap-key-v1\x00"), base64.StdEncoding)
		assertEncodedOutput(t, outputs, "prefs_b64", hkdfExpand32(prk, "concord-prefs-key-v1\x00"), base64.StdEncoding)
	})
	t.Run("canonical context and rsa label", func(t *testing.T) {
		contextVector := requireNamedVector(t, vectors, "context/encode/message-v1/base", "context", "encode")
		contextInput := object(t, contextVector.Input)
		contextBytes := marshalContext(t, stringValue(t, contextInput["tag"]), arrayValue(t, contextInput["context_fields"]))
		assertEncodedOutput(t, acceptedOutputs(t, contextVector), "context_b64", contextBytes, base64.StdEncoding)

		labelVector := requireNamedVector(t, vectors, "rsa-label/derive/channel-key-capsule-v1/base", "rsa-label", "derive")
		labelFields := arrayValue(t, object(t, labelVector.Input)["context_fields"])
		label := marshalContext(t, "channel-key-capsule-v1", labelFields)
		assertEncodedOutput(t, acceptedOutputs(t, labelVector), "label_b64", label, base64.StdEncoding)
	})
	t.Run("aes-gcm encrypt and decrypt", func(t *testing.T) {
		encrypt := requireNamedVector(t, vectors, "aead/encrypt/message-v1/base", "aead", "encrypt")
		input := object(t, encrypt.Input)
		key, nonce := mustBase64(t, input["key_b64"]), mustBase64(t, input["nonce_b64"])
		aad, plaintext := mustBase64(t, input["context_b64"]), mustBase64(t, input["plaintext_b64"])
		block, err := aes.NewCipher(key)
		if err != nil {
			t.Fatal(err)
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			t.Fatal(err)
		}
		sealed := gcm.Seal(nil, nonce, plaintext, aad)
		envelope := []byte("m1:" + base64.StdEncoding.EncodeToString(append(append([]byte{}, nonce...), sealed...)))
		assertEncodedOutput(t, acceptedOutputs(t, encrypt), "envelope_b64", envelope, base64.StdEncoding)

		decrypt := requireNamedVector(t, vectors, "aead/decrypt/message-v1/base", "aead", "decrypt")
		decryptInput := object(t, decrypt.Input)
		wire := mustBase64(t, decryptInput["envelope_b64"])
		if !bytes.HasPrefix(wire, []byte("m1:")) {
			t.Fatalf("unexpected message envelope prefix %q", wire)
		}
		payload, err := base64.StdEncoding.DecodeString(string(wire[3:]))
		if err != nil || len(payload) < 28 {
			t.Fatalf("invalid message envelope: %v", err)
		}
		opened, err := gcm.Open(nil, payload[:12], payload[12:], mustBase64(t, decryptInput["expected_context_b64"]))
		if err != nil {
			t.Fatal(err)
		}
		assertEncodedOutput(t, acceptedOutputs(t, decrypt), "plaintext_b64", opened, base64.StdEncoding)
	})
}

func TestEveryPositiveCryptoBaseMatchesIndependentGoPrimitives(t *testing.T) {
	for name, v := range corpusVectorsByName(t) {
		if !strings.HasSuffix(name, "/base") {
			continue
		}
		switch v.Suite + "/" + v.Operation {
		case "credential/derive":
			t.Run(name, func(t *testing.T) { assertCredentialDerivation(t, v) })
		case "context/encode":
			t.Run(name, func(t *testing.T) {
				input := object(t, v.Input)
				assertEncodedOutput(t, acceptedOutputs(t, v), "context_b64", marshalContext(t, stringValue(t, input["tag"]), arrayValue(t, input["context_fields"])), base64.StdEncoding)
			})
		case "rsa-label/derive":
			t.Run(name, func(t *testing.T) {
				fields := arrayValue(t, object(t, v.Input)["context_fields"])
				assertEncodedOutput(t, acceptedOutputs(t, v), "label_b64", marshalContext(t, "channel-key-capsule-v1", fields), base64.StdEncoding)
			})
		case "aead/encrypt":
			t.Run(name, func(t *testing.T) { assertAEADEncrypt(t, v) })
		case "aead/decrypt":
			t.Run(name, func(t *testing.T) { assertAEADDecrypt(t, v) })
		}
	}
}

func TestPasswordScalarVectorsUseIndependentStrictUTF16(t *testing.T) {
	for name, v := range corpusVectorsByName(t) {
		if v.Suite != "credential" || v.Operation != "password-scalars" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			input := object(t, v.Input)
			utf8, err := strictUTF16Units(stringValue(t, input["utf16_units_hex"]))
			expect := object(t, v.Expect)
			if expect["verdict"] == "reject" {
				if err == nil || expect["stage"] != "validate" || expect["reason"] != "invalid_unicode" {
					t.Fatalf("invalid UTF-16 case got err=%v expect=%#v", err, expect)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertEncodedOutput(t, acceptedOutputs(t, v), "password_utf8_b64", utf8, base64.StdEncoding)
		})
	}
}

func strictUTF16Units(encoded string) ([]byte, error) {
	if len(encoded)%4 != 0 {
		return nil, fmt.Errorf("UTF-16 input is not four-hex-unit aligned")
	}
	runes := make([]rune, 0, len(encoded)/4)
	for index := 0; index < len(encoded); index += 4 {
		unit, err := strconv.ParseUint(encoded[index:index+4], 16, 16)
		if err != nil || strings.ToLower(encoded[index:index+4]) != encoded[index:index+4] {
			return nil, fmt.Errorf("invalid UTF-16 hex unit")
		}
		value := rune(unit)
		if 0xD800 <= value && value <= 0xDBFF {
			if index+4 >= len(encoded) {
				return nil, fmt.Errorf("unpaired high surrogate")
			}
			next, nextErr := strconv.ParseUint(encoded[index+4:index+8], 16, 16)
			if nextErr != nil || next < 0xDC00 || next > 0xDFFF {
				return nil, fmt.Errorf("unpaired high surrogate")
			}
			runes = append(runes, utf16.DecodeRune(value, rune(next)))
			index += 4
			continue
		}
		if 0xDC00 <= value && value <= 0xDFFF {
			return nil, fmt.Errorf("unpaired low surrogate")
		}
		runes = append(runes, value)
	}
	return []byte(string(runes)), nil
}

func assertCredentialDerivation(t *testing.T, v vector) {
	t.Helper()
	input, outputs := object(t, v.Input), acceptedOutputs(t, v)
	password, subject, salt := mustBase64(t, input["password_utf8_b64"]), mustBase64(t, input["subject_b64"]), mustBase64(t, input["salt_b64"])
	generation, ok := input["generation"].(float64)
	if !ok || generation < 1 || generation != float64(uint64(generation)) {
		t.Fatalf("invalid generation %#v", input["generation"])
	}
	root := argon2.IDKey(password, salt, 3, 65536, 4, 32)
	var generationBytes [8]byte
	binary.BigEndian.PutUint64(generationBytes[:], uint64(generation))
	bind := append([]byte("concord-credential-bind-v1\x00"+stringValue(t, input["realm"])), subject...)
	bind = append(bind, generationBytes[:]...)
	prk := hmacSHA256(bind, root)
	assertEncodedOutput(t, outputs, "root_b64", root, base64.StdEncoding)
	assertEncodedOutput(t, outputs, "auth_b64", hkdfExpand32(prk, "concord-auth-secret-v1\x00"), base64.RawURLEncoding)
	assertEncodedOutput(t, outputs, "wrap_b64", hkdfExpand32(prk, "concord-wrap-key-v1\x00"), base64.StdEncoding)
	assertEncodedOutput(t, outputs, "prefs_b64", hkdfExpand32(prk, "concord-prefs-key-v1\x00"), base64.StdEncoding)
}

func assertAEADEncrypt(t *testing.T, v vector) {
	t.Helper()
	input := object(t, v.Input)
	key, nonce := mustBase64(t, input["key_b64"]), mustBase64(t, input["nonce_b64"])
	context, plaintext := mustBase64(t, input["context_b64"]), mustBase64(t, input["plaintext_b64"])
	gcm := mustGCM(t, key)
	sealed := gcm.Seal(nil, nonce, plaintext, context)
	assertEncodedOutput(t, acceptedOutputs(t, v), "envelope_b64", makeEnvelope(t, stringValue(t, input["format"]), nonce, sealed, context), base64.StdEncoding)
}

func assertAEADDecrypt(t *testing.T, v vector) {
	t.Helper()
	input := object(t, v.Input)
	key, context, envelope := mustBase64(t, input["key_b64"]), mustBase64(t, input["expected_context_b64"]), mustBase64(t, input["envelope_b64"])
	nonce, sealed := splitEnvelope(t, stringValue(t, input["format"]), envelope)
	plaintext, err := mustGCM(t, key).Open(nil, nonce, sealed, context)
	if err != nil {
		t.Fatal(err)
	}
	assertEncodedOutput(t, acceptedOutputs(t, v), "plaintext_b64", plaintext, base64.StdEncoding)
}

func mustGCM(t *testing.T, key []byte) cipher.AEAD {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	return gcm
}

func makeEnvelope(t *testing.T, format string, nonce, sealed, context []byte) []byte {
	t.Helper()
	if format == "attachment-v4" {
		_, fields, ok := decodeContext(base64.StdEncoding.EncodeToString(context))
		if !ok || len(fields) != len(contextTagFields["attachment-v4"]) {
			t.Fatal("invalid attachment context")
		}
		fileNonce := mustBase64(t, fields[6])
		header := make([]byte, 28, 28+len(nonce)+len(sealed))
		copy(header, "CV")
		header[2] = 4
		binary.BigEndian.PutUint32(header[4:8], 8388608)
		binary.BigEndian.PutUint32(header[8:12], 1)
		copy(header[12:], fileNonce)
		return append(append(header, nonce...), sealed...)
	}
	prefix := map[string]string{"m1": "m1:", "b1": "b1:", "ek1": "ek1:", "rw1-private": "rw1:", "rw1-preferences": "rw1:"}[format]
	if prefix == "" {
		t.Fatalf("unknown AEAD format %q", format)
	}
	return []byte(prefix + base64.StdEncoding.EncodeToString(append(append([]byte{}, nonce...), sealed...)))
}

func splitEnvelope(t *testing.T, format string, envelope []byte) ([]byte, []byte) {
	t.Helper()
	if format == "attachment-v4" {
		if len(envelope) < 56 || string(envelope[:2]) != "CV" || envelope[2] != 4 {
			t.Fatalf("invalid attachment envelope")
		}
		return envelope[28:40], envelope[40:]
	}
	prefix := map[string]string{"m1": "m1:", "b1": "b1:", "ek1": "ek1:", "rw1-private": "rw1:", "rw1-preferences": "rw1:"}[format]
	if prefix == "" || !bytes.HasPrefix(envelope, []byte(prefix)) {
		t.Fatalf("invalid %s envelope", format)
	}
	payload, err := base64.StdEncoding.DecodeString(string(envelope[len(prefix):]))
	if err != nil || len(payload) < 28 {
		t.Fatalf("invalid text envelope: %v", err)
	}
	return payload[:12], payload[12:]
}

func corpusVectorsByName(t *testing.T) map[string]vector {
	t.Helper()
	var c corpus
	decodeCorpus(t, readFile(t, filepath.Join(repositoryRoot(t), corpusRelativePath)), &c)
	vectors := make(map[string]vector, len(c.Vectors))
	for _, raw := range c.Vectors {
		var v vector
		decodeCorpus(t, raw, &v)
		vectors[v.Name] = v
	}
	return vectors
}

func acceptedOutputs(t *testing.T, v vector) map[string]any {
	t.Helper()
	expect := object(t, v.Expect)
	outputs, ok := expect["outputs"].(map[string]any)
	if !ok || expect["verdict"] != "accept" {
		t.Fatalf("vector %q is not an accepted case", v.Name)
	}
	return outputs
}

func mustBase64(t *testing.T, value any) []byte {
	t.Helper()
	encoded, ok := value.(string)
	if !ok {
		t.Fatalf("expected base64 string, got %#v", value)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || base64.StdEncoding.EncodeToString(decoded) != encoded {
		t.Fatalf("invalid canonical base64 %q: %v", encoded, err)
	}
	return decoded
}

func hmacSHA256(key, message []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(message)
	return mac.Sum(nil)
}

func hkdfExpand32(prk []byte, info string) []byte {
	return hmacSHA256(prk, append([]byte(info), 1))
}

func marshalContext(t *testing.T, tag string, fields []any) []byte {
	t.Helper()
	encoded, err := json.Marshal(append([]any{"concord-e2ee-context", 1, tag}, fields...))
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func assertEncodedOutput(t *testing.T, outputs map[string]any, key string, bytes []byte, encoding *base64.Encoding) {
	t.Helper()
	if got, want := outputs[key], encoding.EncodeToString(bytes); got != want {
		t.Fatalf("%s mismatch: got %q want %q", key, got, want)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, adrRelativePath)); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repository root with docs/ was not found")
		}
		dir = parent
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	// #nosec G304 -- test callers pass only repository paths built from constants.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func sha384Hex(b []byte) string { sum := sha512.Sum384(b); return hex.EncodeToString(sum[:]) }
func decodeCorpus(t *testing.T, data []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(data, dst); err != nil {
		t.Fatalf("invalid corpus JSON: %v", err)
	}
}
func boolSlot(v bool) string {
	if v {
		return "t"
	}
	return "f"
}

func policyVectors(t *testing.T, operation string) map[string]vector {
	t.Helper()
	var c corpus
	decodeCorpus(t, readFile(t, filepath.Join(repositoryRoot(t), corpusRelativePath)), &c)
	got := map[string]vector{}
	for _, raw := range c.Vectors {
		var v vector
		decodeCorpus(t, raw, &v)
		if v.Suite == "policy" && v.Operation == operation {
			got[v.Name] = v
		}
	}
	return got
}

func requireNamedVector(t *testing.T, vectors map[string]vector, name, suite, operation string) vector {
	t.Helper()
	v, ok := vectors[name]
	if !ok {
		t.Fatalf("missing required vector %q", name)
	}
	if v.Suite != suite || v.Operation != operation {
		t.Fatalf("vector %q is %s/%s, want %s/%s", name, v.Suite, v.Operation, suite, operation)
	}
	return v
}

func assertRequiredRejectCategories(t *testing.T, vectors map[string]vector, tags map[string][]string) {
	t.Helper()
	for tag, fields := range tags {
		if tag == "channel-key-capsule-v1" {
			for _, field := range fields {
				assertRejectCategory(t, requireNamedVector(t, vectors, "rsa-label/verify/"+tag+"/alter-"+field, "rsa-label", "verify"), "authenticate", "invalid_label")
			}
			continue
		}
		invalidField := map[string]bool{"content_kind": true, "key_purpose": true, "recovery_purpose": true, "schema_version": true, "flags": true, "chunk_index": true, "total_chunks": true, "chunk_size": true}
		for _, field := range fields {
			stage, reason := "authenticate", "aead_failure"
			if invalidField[field] {
				stage, reason = "validate", "invalid_field"
			}
			assertRejectCategory(t, requireNamedVector(t, vectors, "aead/decrypt/"+tag+"/alter-"+field, "aead", "decrypt"), stage, reason)
		}
		for _, length := range []string{"short-key", "long-key"} {
			assertRejectCategory(t, requireNamedVector(t, vectors, "aead/encrypt/"+tag+"/"+length, "aead", "encrypt"), "validate", "invalid_field")
			assertRejectCategory(t, requireNamedVector(t, vectors, "aead/decrypt/"+tag+"/"+length, "aead", "decrypt"), "validate", "invalid_field")
		}
		for _, length := range []string{"short-nonce", "long-nonce"} {
			assertRejectCategory(t, requireNamedVector(t, vectors, "aead/encrypt/"+tag+"/"+length, "aead", "encrypt"), "validate", "invalid_field")
		}
		assertRejectCategory(t, requireNamedVector(t, vectors, "aead/decrypt/"+tag+"/short-nonce", "aead", "decrypt"), "parse", "bad_envelope")
		assertRejectCategory(t, requireNamedVector(t, vectors, "aead/decrypt/"+tag+"/long-nonce", "aead", "decrypt"), "authenticate", "aead_failure")
		assertRejectCategory(t, requireNamedVector(t, vectors, "aead/decrypt/"+tag+"/wrong-format", "aead", "decrypt"), "validate", "invalid_format_context")
	}
	assertRejectCategory(t, requireNamedVector(t, vectors, "credential/password-scalars/invalid-surrogate", "credential", "password-scalars"), "validate", "invalid_unicode")
	assertRejectCategory(t, requireNamedVector(t, vectors, "credential/descriptor/weak-parameters", "credential", "descriptor"), "validate", "weak_parameters")
	assertRejectCategory(t, requireNamedVector(t, vectors, "credential/descriptor/wrong-realm", "credential", "descriptor"), "validate", "wrong_realm")
	assertRejectCategory(t, requireNamedVector(t, vectors, "aead/decrypt/attachment-v4/tamper-header", "aead", "decrypt"), "authenticate", "aead_failure")
	for name, semantic := range map[string]string{"tamper-flags": "flags", "tamper-chunk-size": "chunk_size", "tamper-total-chunks": "total_chunks"} {
		v := requireNamedVector(t, vectors, "aead/decrypt/attachment-v4/"+name, "aead", "decrypt")
		assertRejectCategory(t, v, "validate", "invalid_field")
		if object(t, v.Mutation)["semantic_field"] != semantic {
			t.Fatalf("vector %q has wrong header semantic field", v.Name)
		}
	}
	for _, field := range []string{"version", "magic"} {
		assertRejectCategory(t, requireNamedVector(t, vectors, "aead/decrypt/attachment-v4/tamper-"+field, "aead", "decrypt"), "parse", "bad_envelope")
	}
	for _, tag := range []string{"account-ek-wrap-v1", "recovery-wrap-v1", "recovery-prefs-wrap-v1"} {
		assertRejectCategory(t, requireNamedVector(t, vectors, "context/encode/"+tag+"/wrong-key-purpose", "context", "encode"), "validate", "invalid_purpose")
	}
	for _, name := range []string{"short-subject", "short-salt", "invalid-password-utf8"} {
		assertRejectCategory(t, requireNamedVector(t, vectors, "credential/derive/"+name, "credential", "derive"), "validate", "invalid_field")
	}
	for _, name := range []string{"noncanonical-whitespace", "invalid-subject-base64", "wrong-discriminator", "zero-generation", "trailing-field"} {
		assertRejectCategory(t, requireNamedVector(t, vectors, "credential/descriptor/"+name, "credential", "descriptor"), "parse", "malformed_descriptor")
	}
	assertRejectCategory(t, requireNamedVector(t, vectors, "rsa-label/derive/channel-key-capsule-v1/invalid-recipient-id", "rsa-label", "derive"), "validate", "invalid_field")
	assertRejectCategory(t, requireNamedVector(t, vectors, "context/encode/message-v1/unknown-tag", "context", "encode"), "validate", "unknown_tag")
	assertRejectCategory(t, requireNamedVector(t, vectors, "context/encode/message-v1/invalid-realm", "context", "encode"), "validate", "invalid_field")
	assertRejectCategory(t, requireNamedVector(t, vectors, "context/parse/message-v1/unknown-tag", "context", "parse"), "validate", "unknown_tag")
	assertRejectCategory(t, requireNamedVector(t, vectors, "context/parse/message-v1/noncanonical-whitespace", "context", "parse"), "parse", "malformed_context")
	assertRejectCategory(t, requireNamedVector(t, vectors, "context/parse/message-v1/invalid-realm", "context", "parse"), "validate", "invalid_field")
	assertRejectCategory(t, requireNamedVector(t, vectors, "context/parse/account-ek-wrap-v1/wrong-key-purpose", "context", "parse"), "validate", "invalid_purpose")
	for _, name := range []string{"invalid-flags", "invalid-chunk-index", "fractional-chunk-index", "fractional-total-chunks"} {
		assertRejectCategory(t, requireNamedVector(t, vectors, "context/parse/attachment-v4/"+name, "context", "parse"), "validate", "invalid_field")
	}
}

func assertRejectCategory(t *testing.T, v vector, stage, reason string) {
	t.Helper()
	expect := object(t, v.Expect)
	if expect["verdict"] != "reject" || expect["stage"] != stage || expect["reason"] != reason {
		t.Fatalf("vector %q reject got %#v want %s/%s", v.Name, expect, stage, reason)
	}
}

func assertRequiredCandidateMutationManifest(t *testing.T, vectors map[string]vector) {
	t.Helper()
	required := []struct {
		name, parent, semantic, kind string
	}{
		{"credential/derive/short-subject", "credential/derive/base", "subject_b64", "replace"},
		{"credential/derive/short-salt", "credential/derive/base", "salt_b64", "replace"},
		{"credential/derive/invalid-password-utf8", "credential/derive/base", "password_utf8_b64", "replace"},
		{"credential/descriptor/noncanonical-whitespace", "credential/descriptor/base", "descriptor_b64", "insert"},
		{"credential/descriptor/invalid-subject-base64", "credential/descriptor/base", "subject_b64", "replace"},
		{"credential/descriptor/wrong-discriminator", "credential/descriptor/base", "protocol", "replace"},
		{"credential/descriptor/zero-generation", "credential/descriptor/base", "generation", "replace"},
		{"credential/descriptor/trailing-field", "credential/descriptor/base", "descriptor_b64", "insert"},
		{"rsa-label/derive/channel-key-capsule-v1/invalid-recipient-id", "rsa-label/derive/channel-key-capsule-v1/base", "recipient_id", "replace"},
		{"context/encode/message-v1/unknown-tag", "context/encode/message-v1/base", "tag", "replace"},
		{"context/encode/message-v1/invalid-realm", "context/encode/message-v1/base", "realm", "replace"},
		{"context/parse/message-v1/unknown-tag", "context/parse/message-v1/base", "tag", "replace"},
		{"context/parse/message-v1/noncanonical-whitespace", "context/parse/message-v1/base", "context_b64", "insert"},
		{"context/parse/message-v1/invalid-realm", "context/parse/message-v1/base", "realm", "replace"},
		{"context/parse/account-ek-wrap-v1/wrong-key-purpose", "context/parse/account-ek-wrap-v1/base", "key_purpose", "replace"},
		{"context/parse/attachment-v4/invalid-flags", "context/parse/attachment-v4/base", "flags", "replace"},
		{"context/parse/attachment-v4/invalid-chunk-index", "context/parse/attachment-v4/base", "chunk_index", "replace"},
		{"context/parse/attachment-v4/fractional-chunk-index", "context/parse/attachment-v4/base", "chunk_index", "replace"},
		{"context/parse/attachment-v4/fractional-total-chunks", "context/parse/attachment-v4/base", "total_chunks", "replace"},
	}
	for _, required := range required {
		v, ok := vectors[required.name]
		if !ok {
			t.Fatalf("missing required candidate mutation %q", required.name)
		}
		mutation := object(t, v.Mutation)
		if v.NegativeOf != required.parent || mutation["semantic_field"] != required.semantic || mutation["kind"] != required.kind {
			t.Fatalf("candidate mutation %q got parent=%q mutation=%#v", required.name, v.NegativeOf, mutation)
		}
	}

	assertBase64Length(t, vectors["credential/derive/short-subject"], "subject_b64", 31)
	assertBase64Length(t, vectors["credential/derive/short-salt"], "salt_b64", 15)
	if got := mustBase64(t, object(t, vectors["credential/derive/invalid-password-utf8"].Input)["password_utf8_b64"]); utf8.Valid(got) {
		t.Fatal("invalid-password-utf8 must carry invalid UTF-8 candidate bytes")
	}
	assertDescriptorCandidateMutations(t, vectors)
	for _, name := range []string{
		"context/parse/message-v1/unknown-tag",
		"context/parse/message-v1/noncanonical-whitespace",
		"context/parse/message-v1/invalid-realm",
		"context/parse/account-ek-wrap-v1/wrong-key-purpose",
		"context/parse/attachment-v4/invalid-flags",
		"context/parse/attachment-v4/invalid-chunk-index",
		"context/parse/attachment-v4/fractional-chunk-index",
		"context/parse/attachment-v4/fractional-total-chunks",
	} {
		assertContextParseCandidateMutation(t, vectors["context/parse/"+strings.TrimPrefix(name, "context/parse/")], vectors)
	}
	for name, index := range map[string]int{
		"context/parse/attachment-v4/fractional-chunk-index":  8,
		"context/parse/attachment-v4/fractional-total-chunks": 9,
	} {
		candidate := mustBase64(t, object(t, vectors[name].Input)["context_b64"])
		var fields []any
		if json.Unmarshal(candidate, &fields) != nil || len(fields) <= index+3 || fields[index+3] != 0.5 {
			t.Fatalf("vector %q must carry fractional 0.5 at context field %d", name, index)
		}
	}
}

func assertBase64Length(t *testing.T, v vector, key string, want int) {
	t.Helper()
	if got := len(mustBase64(t, object(t, v.Input)[key])); got != want {
		t.Fatalf("vector %q decoded %s length got %d want %d", v.Name, key, got, want)
	}
}

func assertDescriptorCandidateMutations(t *testing.T, vectors map[string]vector) {
	t.Helper()
	base := mustBase64(t, object(t, vectors["credential/descriptor/base"].Input)["descriptor_b64"])
	whitespace := mustBase64(t, object(t, vectors["credential/descriptor/noncanonical-whitespace"].Input)["descriptor_b64"])
	if bytes.Equal(base, whitespace) || !bytes.HasPrefix(whitespace, []byte(" ")) {
		t.Fatal("noncanonical descriptor must add leading whitespace")
	}
	var baseFields, whitespaceFields []any
	if json.Unmarshal(base, &baseFields) != nil || json.Unmarshal(whitespace, &whitespaceFields) != nil || !sameJSONValues(baseFields, whitespaceFields) {
		t.Fatal("noncanonical descriptor must retain the same parsed fields")
	}
	badSubject := mustBase64(t, object(t, vectors["credential/descriptor/invalid-subject-base64"].Input)["descriptor_b64"])
	var fields []any
	if json.Unmarshal(badSubject, &fields) != nil || len(fields) != 8 {
		t.Fatal("invalid-subject descriptor must remain an eight-field JSON array")
	}
	if len(baseFields) != len(fields) || fields[2] == baseFields[2] {
		t.Fatal("invalid-subject descriptor must change its subject field")
	}
	for index := range fields {
		if index != 2 && !jsonValueEqual(baseFields[index], fields[index]) {
			t.Fatalf("invalid-subject descriptor changed field %d in addition to subject", index)
		}
	}
	encoded, ok := fields[2].(string)
	if !ok {
		t.Fatal("invalid-subject descriptor must carry a string subject candidate")
	}
	if _, err := base64.StdEncoding.DecodeString(encoded); err == nil {
		t.Fatal("invalid-subject descriptor must carry a non-base64 subject candidate")
	}
	assertDescriptorFieldMutation(t, baseFields, mustDescriptorFields(t, vectors["credential/descriptor/wrong-discriminator"]), 0, "wrong discriminator")
	assertDescriptorFieldMutation(t, baseFields, mustDescriptorFields(t, vectors["credential/descriptor/zero-generation"]), 3, "zero generation")
	trailing := mustDescriptorFields(t, vectors["credential/descriptor/trailing-field"])
	if len(trailing) != len(baseFields)+1 || trailing[len(baseFields)] != "extra" || !sameJSONValues(baseFields, trailing[:len(baseFields)]) {
		t.Fatal("trailing descriptor field must append exactly one field")
	}
}

func mustDescriptorFields(t *testing.T, v vector) []any {
	t.Helper()
	candidate := mustBase64(t, object(t, v.Input)["descriptor_b64"])
	var fields []any
	if json.Unmarshal(candidate, &fields) != nil {
		t.Fatalf("vector %q has invalid JSON descriptor candidate", v.Name)
	}
	return fields
}

func assertDescriptorFieldMutation(t *testing.T, base, candidate []any, index int, name string) {
	t.Helper()
	if len(base) != len(candidate) || index >= len(base) || jsonValueEqual(base[index], candidate[index]) {
		t.Fatalf("%s descriptor candidate did not change required field", name)
	}
	for i := range base {
		if i != index && !jsonValueEqual(base[i], candidate[i]) {
			t.Fatalf("%s descriptor candidate changed field %d in addition to %d", name, i, index)
		}
	}
}

func assertContextParseCandidateMutation(t *testing.T, v vector, vectors map[string]vector) {
	t.Helper()
	parent := vectors[v.NegativeOf]
	parentBytes := mustBase64(t, object(t, parent.Input)["context_b64"])
	childBytes := mustBase64(t, object(t, v.Input)["context_b64"])
	var parentFields, childFields []any
	if json.Unmarshal(parentBytes, &parentFields) != nil || json.Unmarshal(childBytes, &childFields) != nil || len(parentFields) < 3 || len(childFields) < 3 {
		t.Fatalf("vector %q must carry JSON context candidates", v.Name)
	}
	semantic := stringValue(t, object(t, v.Mutation)["semantic_field"])
	if semantic == "context_b64" {
		canonical, err := json.Marshal(childFields)
		if err != nil || bytes.Equal(canonical, childBytes) || !sameJSONValues(parentFields, childFields) {
			t.Fatalf("vector %q must differ only by noncanonical JSON bytes", v.Name)
		}
		return
	}
	if semantic == "tag" {
		if parentFields[2] == childFields[2] || !sameJSONValues(parentFields[3:], childFields[3:]) {
			t.Fatalf("vector %q must change only its context tag", v.Name)
		}
		return
	}
	tag, _ := parentFields[2].(string)
	fieldNames := contextTagFields[tag]
	for index, field := range fieldNames {
		if !jsonValueEqual(parentFields[index+3], childFields[index+3]) && field != semantic {
			t.Fatalf("vector %q changed context field %q while declaring %q", v.Name, field, semantic)
		}
	}
}

func assertNamedPolicyPredicates(t *testing.T, vectors map[string]vector) {
	t.Helper()
	legacyBase := map[string]any{"device_state": "existing", "account_protocol": "legacy", "authority_proof": "verified", "realm_match": true, "account_match": true, "freshness": "current", "not_expired": true, "control_plane_continuation": true}
	legacy := map[string]map[string]any{
		"eligible":                   legacyBase,
		"forged-split":               policyInput(legacyBase, map[string]any{"account_protocol": "split"}),
		"invalid-proof":              policyInput(legacyBase, map[string]any{"authority_proof": "invalid"}),
		"existing-device-no-proof":   policyInput(legacyBase, map[string]any{"authority_proof": "absent"}),
		"stale-post-upgrade":         policyInput(legacyBase, map[string]any{"freshness": "stale"}),
		"wrong-account":              policyInput(legacyBase, map[string]any{"account_match": false}),
		"wrong-realm":                policyInput(legacyBase, map[string]any{"realm_match": false}),
		"expired-proof":              policyInput(legacyBase, map[string]any{"not_expired": false}),
		"no-continuation":            policyInput(legacyBase, map[string]any{"control_plane_continuation": false}),
		"fresh-device-no-proof":      policyInput(legacyBase, map[string]any{"device_state": "fresh", "authority_proof": "absent"}),
		"fresh-device-invalid-proof": policyInput(legacyBase, map[string]any{"device_state": "fresh", "authority_proof": "invalid"}),
	}
	for name, predicate := range legacy {
		v := requireNamedVector(t, vectors, "policy/legacy-continuation/"+name, "policy", "legacy-continuation")
		input := object(t, v.Input)
		mustEqualPolicyInput(t, input, predicate)
		decision, reason := legacyDecision(
			stringValue(t, input["account_protocol"]), stringValue(t, input["authority_proof"]), boolValue(t, input["realm_match"]), boolValue(t, input["account_match"]), stringValue(t, input["freshness"]), boolValue(t, input["not_expired"]), boolValue(t, input["control_plane_continuation"]),
		)
		assertPolicyExpect(t, v.Expect, decision, reason)
	}
	upgradeBase := map[string]any{"prior_state": "legacy", "authorization_state": "fresh", "request_match": true, "mfa_complete": true, "commit_state": "none"}
	upgrade := map[string]map[string]any{
		"fresh-authorization":    upgradeBase,
		"fresh-request-conflict": policyInput(upgradeBase, map[string]any{"request_match": false}),
		"already-split":          policyInput(upgradeBase, map[string]any{"prior_state": "split"}),
		"replay":                 policyInput(upgradeBase, map[string]any{"prior_state": "split", "authorization_state": "consumed", "commit_state": "committed"}),
		"request-conflict":       policyInput(upgradeBase, map[string]any{"prior_state": "split", "authorization_state": "consumed", "request_match": false, "commit_state": "committed"}),
		"mfa-required":           policyInput(upgradeBase, map[string]any{"mfa_complete": false}),
		"ambiguous-commit":       policyInput(upgradeBase, map[string]any{"commit_state": "ambiguous"}),
		"expired-authorization":  policyInput(upgradeBase, map[string]any{"authorization_state": "expired"}),
		"missing-authorization":  policyInput(upgradeBase, map[string]any{"authorization_state": "absent"}),
	}
	for name, predicate := range upgrade {
		v := requireNamedVector(t, vectors, "policy/upgrade-outcome/"+name, "policy", "upgrade-outcome")
		input := object(t, v.Input)
		mustEqualPolicyInput(t, input, predicate)
		decision, reason := upgradeDecision(stringValue(t, input["prior_state"]), stringValue(t, input["authorization_state"]), boolValue(t, input["request_match"]), boolValue(t, input["mfa_complete"]), stringValue(t, input["commit_state"]))
		assertPolicyExpect(t, v.Expect, decision, reason)
	}
}

func policyInput(base, changes map[string]any) map[string]any {
	cloned := make(map[string]any, len(base))
	for key, value := range base {
		cloned[key] = value
	}
	for key, value := range changes {
		cloned[key] = value
	}
	return cloned
}

func mustEqualPolicyInput(t *testing.T, got, want map[string]any) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("policy input keys got %#v want %#v", got, want)
	}
	for key, value := range want {
		mustEqualJSON(t, got, key, value)
	}
}

func object(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var got map[string]any
	decodeCorpus(t, raw, &got)
	return got
}
func stringValue(t *testing.T, value any) string {
	t.Helper()
	got, ok := value.(string)
	if !ok {
		t.Fatalf("expected JSON string, got %#v", value)
	}
	return got
}
func boolValue(t *testing.T, value any) bool {
	t.Helper()
	got, ok := value.(bool)
	if !ok {
		t.Fatalf("expected JSON bool, got %#v", value)
	}
	return got
}
func arrayValue(t *testing.T, value any) []any {
	t.Helper()
	got, ok := value.([]any)
	if !ok {
		t.Fatalf("expected JSON array, got %#v", value)
	}
	return got
}
func mustEqualJSON(t *testing.T, got map[string]any, key string, want any) {
	t.Helper()
	if got[key] != want {
		t.Fatalf("%s: got %#v want %#v", key, got[key], want)
	}
}
func assertPolicyExpect(t *testing.T, raw json.RawMessage, decision, reason string) {
	t.Helper()
	expect := object(t, raw)
	if expect["verdict"] != "accept" {
		t.Fatalf("policy vector has non-accept verdict: %#v", expect)
	}
	outputs, ok := expect["outputs"].(map[string]any)
	if !ok || outputs["decision"] != decision || outputs["reason"] != reason {
		t.Fatalf("policy output got %#v want %s/%s", expect, decision, reason)
	}
}

func legacyDecision(protocol, proof string, realm, account bool, freshness string, notExpired, continuation bool) (string, string) {
	switch {
	case protocol != "legacy":
		return "deny", "not-legacy"
	case proof == "absent":
		return "deny", "missing-authority-proof"
	case proof != "verified":
		return "deny", "invalid-authority-proof"
	case freshness != "current":
		return "deny", "stale-authority-proof"
	case !account:
		return "deny", "wrong-account"
	case !realm:
		return "deny", "wrong-realm"
	case !notExpired:
		return "deny", "expired-proof"
	case !continuation:
		return "deny", "no-continuation"
	default:
		return "submit-raw-password", "eligible"
	}
}
func upgradeDecision(prior, authorization string, match, mfa bool, commit string) (string, string) {
	switch {
	case commit == "ambiguous":
		return "reconcile", "ambiguous-commit"
	case !mfa:
		return "deny", "mfa-required"
	case commit == "committed" && prior == "split" && authorization == "consumed" && match:
		return "idempotent", "replay"
	case commit == "committed" && prior == "split" && authorization == "consumed" && !match:
		return "conflict", "request-conflict"
	case commit == "committed":
		return "reconcile", "ambiguous-commit"
	case prior == "split":
		return "deny", "not-legacy"
	case authorization == "consumed":
		return "reconcile", "ambiguous-commit"
	case authorization == "expired":
		return "deny", "expired-authorization"
	case authorization == "absent":
		return "deny", "missing-authorization"
	case !match:
		return "conflict", "request-conflict"
	default:
		return "commit", "fresh-authorization"
	}
}

func assertCanonicalTopOrder(t *testing.T, data []byte) {
	t.Helper()
	want := []string{"schema", "version", "generator_sha384", "vectors_sha384", "vectors"}
	if got := objectKeys(t, data); !sameStrings(got, want) {
		t.Fatalf("top-level key order got %v want %v", got, want)
	}
}
func assertVectorOrder(t *testing.T, raw json.RawMessage, v vector) {
	t.Helper()
	want := []string{"name", "suite", "operation", "cite", "input"}
	var expect map[string]any
	decodeCorpus(t, v.Expect, &expect)
	if expect["verdict"] == "reject" {
		want = append(want, "negative_of", "mutation")
	}
	want = append(want, "expect")
	if got := objectKeys(t, raw); !sameStrings(got, want) {
		t.Fatalf("vector %q key order got %v want %v", v.Name, got, want)
	}
}

func assertClosedOperationShape(t *testing.T, v vector) {
	t.Helper()
	inputKeys := map[string][]string{
		"credential/password-scalars":  {"utf16_units_hex"},
		"credential/derive":            {"password_utf8_b64", "realm", "subject_b64", "generation", "salt_b64"},
		"credential/descriptor":        {"authenticated_realm", "descriptor_b64"},
		"context/encode":               {"tag", "context_fields"},
		"context/parse":                {"context_b64"},
		"aead/encrypt":                 {"format", "key_b64", "nonce_b64", "context_b64", "plaintext_b64"},
		"aead/decrypt":                 {"format", "key_b64", "expected_context_b64", "envelope_b64"},
		"rsa-label/derive":             {"context_fields"},
		"rsa-label/verify":             {"context_fields", "candidate_label_b64"},
		"policy/descriptor-uniformity": {"account_class", "probe_public_account_uuid", "descriptor_b64", "proof_verdict"},
		"policy/legacy-continuation":   {"device_state", "account_protocol", "authority_proof", "realm_match", "account_match", "freshness", "not_expired", "control_plane_continuation"},
		"policy/upgrade-outcome":       {"prior_state", "authorization_state", "request_match", "mfa_complete", "commit_state"},
	}
	outputKeys := map[string][]string{
		"credential/password-scalars":  {"password_utf8_b64"},
		"credential/derive":            {"root_b64", "auth_b64", "wrap_b64", "prefs_b64"},
		"credential/descriptor":        {"realm", "subject_b64", "generation", "salt_b64", "m_kib", "t", "p"},
		"context/encode":               {"context_b64"},
		"context/parse":                {"tag", "context_fields"},
		"aead/encrypt":                 {"envelope_b64"},
		"aead/decrypt":                 {"plaintext_b64"},
		"rsa-label/derive":             {"label_b64"},
		"rsa-label/verify":             {},
		"policy/descriptor-uniformity": {"descriptor_shape", "failed_proof_status", "failed_proof_shape", "failed_proof_error_class", "attempt_charge"},
		"policy/legacy-continuation":   {"decision", "reason"},
		"policy/upgrade-outcome":       {"decision", "reason"},
	}
	want, ok := inputKeys[v.Suite+"/"+v.Operation]
	if !ok {
		t.Fatalf("vector %q uses unknown suite/operation %s/%s", v.Name, v.Suite, v.Operation)
	}
	if got := objectKeys(t, v.Input); !sameStrings(got, want) {
		t.Fatalf("vector %q input keys got %v want %v", v.Name, got, want)
	}
	assertCanonicalBase64Input(t, v)
	var expect map[string]any
	decodeCorpus(t, v.Expect, &expect)
	switch expect["verdict"] {
	case "accept":
		if got := objectKeys(t, v.Expect); !sameStrings(got, []string{"verdict", "outputs"}) {
			t.Fatalf("vector %q accepts with invalid expect keys %v", v.Name, got)
		}
		var rawExpect struct {
			Outputs json.RawMessage `json:"outputs"`
		}
		decodeCorpus(t, v.Expect, &rawExpect)
		if got := objectKeys(t, rawExpect.Outputs); !sameStrings(got, outputKeys[v.Suite+"/"+v.Operation]) {
			t.Fatalf("vector %q output keys got %v want %v", v.Name, got, outputKeys[v.Suite+"/"+v.Operation])
		}
	case "reject":
		if got := objectKeys(t, v.Expect); !sameStrings(got, []string{"verdict", "stage", "reason"}) {
			t.Fatalf("vector %q rejects with invalid expect keys %v", v.Name, got)
		}
	default:
		t.Fatalf("vector %q has invalid verdict %#v", v.Name, expect["verdict"])
	}
}

func assertVectorMutation(t *testing.T, v vector, seen map[string]vector) {
	t.Helper()
	if v.NegativeOf == "" {
		return
	}
	parent, ok := seen[v.NegativeOf]
	if !ok {
		t.Fatalf("vector %q references missing parent %q", v.Name, v.NegativeOf)
	}
	if parent.Suite != v.Suite || parent.Operation != v.Operation {
		t.Fatalf("vector %q parent has different suite/operation", v.Name)
	}
	m := object(t, v.Mutation)
	if got := objectKeys(t, v.Mutation); !sameStrings(got, []string{"kind", "path", "semantic_field"}) {
		t.Fatalf("vector %q mutation keys got %v", v.Name, got)
	}
	kind, ok := m["kind"].(string)
	if !ok || !map[string]bool{"replace": true, "remove": true, "insert": true, "reorder": true, "truncate": true}[kind] {
		t.Fatalf("vector %q has invalid mutation kind", v.Name)
	}
	path, ok := m["path"].(string)
	if !ok || !strings.HasPrefix(path, "/input/") {
		t.Fatalf("vector %q has invalid mutation path", v.Name)
	}
	semantic, ok := m["semantic_field"].(string)
	if !ok || semantic == "" {
		t.Fatalf("vector %q has invalid semantic field", v.Name)
	}
	if bytes.Equal(v.Input, parent.Input) {
		t.Fatalf("vector %q mutation does not change the consumed input", v.Name)
	}
	if path == "/input/"+semantic {
		parentInput := object(t, parent.Input)
		childInput := object(t, v.Input)
		if parentInput[semantic] == childInput[semantic] {
			t.Fatalf("vector %q mutation did not change %q", v.Name, semantic)
		}
	}
	assertSemanticContextMutation(t, parent, v, semantic)
}

func assertCanonicalBase64Input(t *testing.T, v vector) {
	t.Helper()
	input := object(t, v.Input)
	for key, value := range input {
		if !strings.HasSuffix(key, "_b64") {
			continue
		}
		encoded, ok := value.(string)
		if !ok {
			t.Fatalf("vector %q has non-string %s", v.Name, key)
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || base64.StdEncoding.EncodeToString(decoded) != encoded {
			t.Fatalf("vector %q has non-canonical base64 %s", v.Name, key)
		}
	}
}

// assertSemanticContextMutation independently checks the bytes that carry a
// registered context. Invalid candidate bytes are deliberately left to their
// operation-specific rejecting vector; a valid, altered context must change
// exactly the declared positional field.
func assertSemanticContextMutation(t *testing.T, parent, child vector, semantic string) {
	t.Helper()
	parentTag, parentFields, parentOK := vectorContext(parent)
	childTag, childFields, childOK := vectorContext(child)
	if !parentOK || !childOK || parentTag != childTag {
		return
	}
	if parentTag == "attachment-v4" && sameJSONValues(parentFields, childFields) && attachmentHeaderField(semantic) != nil {
		assertAttachmentHeaderMutation(t, parent, child, semantic)
		return
	}
	fieldNames, known := contextTagFields[parentTag]
	if !known || !containsString(fieldNames, semantic) {
		return
	}
	if len(parentFields) != len(fieldNames) || len(childFields) != len(fieldNames) {
		t.Fatalf("vector %q has invalid %s context field count", child.Name, parentTag)
	}
	changed := 0
	for index := range fieldNames {
		if !jsonValueEqual(parentFields[index], childFields[index]) {
			if fieldNames[index] != semantic {
				t.Fatalf("vector %q changed context field %q while declaring %q", child.Name, fieldNames[index], semantic)
			}
			changed++
		}
	}
	if changed != 1 {
		t.Fatalf("vector %q changed %d context fields while declaring %q", child.Name, changed, semantic)
	}
}

func vectorContext(v vector) (string, []any, bool) {
	input := objectNoFail(v.Input)
	switch v.Suite + "/" + v.Operation {
	case "context/encode":
		tag, tagOK := input["tag"].(string)
		fields, fieldsOK := input["context_fields"].([]any)
		return tag, fields, tagOK && fieldsOK
	case "rsa-label/derive", "rsa-label/verify":
		fields, fieldsOK := input["context_fields"].([]any)
		return "channel-key-capsule-v1", fields, fieldsOK
	case "context/parse", "aead/encrypt":
		encoded, ok := input["context_b64"].(string)
		if !ok {
			return "", nil, false
		}
		return decodeContext(encoded)
	case "aead/decrypt":
		encoded, ok := input["expected_context_b64"].(string)
		if !ok {
			return "", nil, false
		}
		return decodeContext(encoded)
	default:
		return "", nil, false
	}
}

func objectNoFail(raw json.RawMessage) map[string]any {
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return value
}

func decodeContext(encoded string) (string, []any, bool) {
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || base64.StdEncoding.EncodeToString(decoded) != encoded {
		return "", nil, false
	}
	var array []any
	if json.Unmarshal(decoded, &array) != nil || len(array) < 3 {
		return "", nil, false
	}
	if array[0] != "concord-e2ee-context" || array[1] != float64(1) {
		return "", nil, false
	}
	tag, ok := array[2].(string)
	if !ok {
		return "", nil, false
	}
	canonical, err := json.Marshal(array)
	if err != nil || !bytes.Equal(decoded, canonical) {
		return "", nil, false
	}
	return tag, array[3:], true
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func jsonValueEqual(left, right any) bool {
	leftBytes, leftErr := json.Marshal(left)
	rightBytes, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftBytes, rightBytes)
}

func sameJSONValues(left, right []any) bool {
	return len(left) == len(right) && jsonValueEqual(left, right)
}

func attachmentHeaderField(semantic string) []int {
	ranges := map[string][]int{
		"magic":          {0, 2},
		"version":        {2, 3},
		"flags":          {3, 4},
		"chunk_size":     {4, 8},
		"total_chunks":   {8, 12},
		"file_nonce_b64": {12, 28},
	}
	return ranges[semantic]
}

func assertAttachmentHeaderMutation(t *testing.T, parent, child vector, semantic string) {
	t.Helper()
	if parent.Suite != "aead" || parent.Operation != "decrypt" || child.Suite != "aead" || child.Operation != "decrypt" {
		t.Fatalf("vector %q declared attachment header mutation outside AEAD decrypt", child.Name)
	}
	parentEnvelope, parentOK := base64Input(parent, "envelope_b64")
	childEnvelope, childOK := base64Input(child, "envelope_b64")
	if !parentOK || !childOK || len(parentEnvelope) < 28 || len(childEnvelope) != len(parentEnvelope) {
		t.Fatalf("vector %q has invalid attachment envelope mutation", child.Name)
	}
	target := attachmentHeaderField(semantic)
	if target == nil {
		t.Fatalf("vector %q has unknown attachment header semantic %q", child.Name, semantic)
	}
	if bytes.Equal(parentEnvelope[target[0]:target[1]], childEnvelope[target[0]:target[1]]) {
		t.Fatalf("vector %q did not change attachment header %q", child.Name, semantic)
	}
	if !bytes.Equal(parentEnvelope[:target[0]], childEnvelope[:target[0]]) || !bytes.Equal(parentEnvelope[target[1]:], childEnvelope[target[1]:]) {
		t.Fatalf("vector %q changed bytes outside attachment header %q", child.Name, semantic)
	}
	if semantic == "file_nonce_b64" {
		changed := 0
		for i := target[0]; i < target[1]; i++ {
			if parentEnvelope[i] != childEnvelope[i] {
				changed++
			}
		}
		if changed != 1 {
			t.Fatalf("vector %q changed %d attachment fileNonce bytes, want one", child.Name, changed)
		}
		_, fields, contextOK := vectorContext(parent)
		if !contextOK || len(fields) != len(contextTagFields["attachment-v4"]) {
			t.Fatalf("vector %q has no valid parent attachment context", child.Name)
		}
		candidateFields := append([]any(nil), fields...)
		candidateFields[6] = base64.StdEncoding.EncodeToString(childEnvelope[12:28])
		candidateAAD := marshalContext(t, "attachment-v4", candidateFields)
		key, keyOK := base64Input(child, "key_b64")
		if !keyOK {
			t.Fatalf("vector %q has invalid key", child.Name)
		}
		if _, err := mustGCM(t, key).Open(nil, childEnvelope[28:40], childEnvelope[40:], candidateAAD); err == nil {
			t.Fatalf("vector %q header-derived candidate AAD unexpectedly authenticated", child.Name)
		}
	}
}

func base64Input(v vector, key string) ([]byte, bool) {
	encoded, ok := objectNoFail(v.Input)[key].(string)
	if !ok {
		return nil, false
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	return decoded, err == nil && base64.StdEncoding.EncodeToString(decoded) == encoded
}
func assertNoDuplicateObjectKeys(t *testing.T, data []byte) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	walkJSON(t, dec)
	if dec.More() {
		t.Fatal("trailing JSON value")
	}
}
func walkJSON(t *testing.T, dec *json.Decoder) {
	t.Helper()
	token, err := dec.Token()
	if err != nil {
		t.Fatal(err)
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				t.Fatal(err)
			}
			key, ok := k.(string)
			if !ok {
				t.Fatal("object key is not string")
			}
			if seen[key] {
				t.Fatalf("duplicate JSON key %q", key)
			}
			seen[key] = true
			walkJSON(t, dec)
		}
		if end, err := dec.Token(); err != nil || end != json.Delim('}') {
			t.Fatal("unclosed object")
		}
	case '[':
		for dec.More() {
			walkJSON(t, dec)
		}
		if end, err := dec.Token(); err != nil || end != json.Delim(']') {
			t.Fatal("unclosed array")
		}
	}
}
func objectKeys(t *testing.T, data []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	start, err := dec.Token()
	if err != nil || start != json.Delim('{') {
		t.Fatal("expected object")
	}
	var keys []string
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, stringValue(t, token))
		var discard any
		if err := dec.Decode(&discard); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := dec.Token(); err != nil {
		t.Fatal(err)
	}
	return keys
}
func sameStrings(got, want []string) bool {
	return len(got) == len(want) && strings.Join(got, "\x00") == strings.Join(want, "\x00")
}
