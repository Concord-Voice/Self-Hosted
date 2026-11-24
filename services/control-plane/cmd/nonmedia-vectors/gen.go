// Command nonmedia-vectors creates the test-only ADR-0051 corpus. It has no production dependency.
package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/hkdf"
)

const (
	corpusPath               = "docs/design/nonmedia-e2ee-v1-vectors.json"
	adrPath                  = "docs/adr/0051-nonmedia-e2ee-context-and-credential-domains.md"
	generatorPath            = "services/control-plane/cmd/nonmedia-vectors/gen.go"
	realm                    = "11111111-1111-4111-8111-111111111111"
	otherRealm               = "22222222-2222-4222-8222-222222222222"
	account                  = "33333333-3333-4333-8333-333333333333"
	otherAccount             = "44444444-4444-4444-8444-444444444444"
	objectID                 = "55555555-5555-4555-8555-555555555555"
	otherObject              = "66666666-6666-4666-8666-666666666666"
	actor                    = "77777777-7777-4777-8777-777777777777"
	otherActor               = "88888888-8888-4888-8888-888888888888"
	contextCitation          = "ADR-0051 Canonical non-authority context"
	contextEncodePrefix      = "context/encode/"
	attachmentV4             = "attachment-v4"
	splitAuthWrapV1          = "split-auth-wrap-v1"
	domainSeparationCitation = "ADR-0051 Credential domain separation"
	passwordScalars          = "password-scalars"
	descriptorBase           = "credential/descriptor/base"
	descriptorInputPath      = "/input/descriptor_b64"
	serverSelectedV2         = "server-selected-v2"
	accountKeyPurpose        = "account-rsa-oaep4096-pss-sha256"
	recoveryPrefsWrapV1      = "recovery-prefs-wrap-v1"
	preferencesPurpose       = "preferences-aes256-gcm"
	aeadEncryptPrefix        = "aead/encrypt/"
	aeadDecryptPrefix        = "aead/decrypt/"
	shortNonce               = "short-nonce"
	envelopeInputPath        = "/input/envelope_b64"
	rsaLabelDerivePrefix     = "rsa-label/derive/"
	rsaLabelSuite            = "rsa-label"
	invalidUUID              = "not-a-uuid"
	messageParseBase         = "context/parse/message-v1/base"
	contextInputPath         = "/input/context_b64"
	attachmentParseBase      = "context/parse/attachment-v4/base"
	ambiguousCommit          = "ambiguous-commit"
	requestConflict          = "request-conflict"
)

type entry struct {
	key   string
	value any
}
type ordered []entry

func field(key string, value any) entry { return entry{key, value} }
func (o ordered) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, e := range o {
		if i != 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(e.key)
		v, err := json.Marshal(e.value)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}
func obj(e ...entry) ordered { return ordered(e) }
func b64(b []byte) string    { return base64.StdEncoding.EncodeToString(b) }
func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}
func jsonBytes(v any) []byte   { return must(json.Marshal(v)) }
func sha384Of(b []byte) string { h := sha512.Sum384(b); return hex.EncodeToString(h[:]) }
func fixed(n int, start byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = start + byte(i)
	}
	return b
}

type caseVector struct {
	name, suite, operation, cite string
	input                        ordered
	negativeOf                   string
	mutation                     ordered
	expect                       ordered
}

func (v caseVector) bytes() []byte {
	o := obj(field("name", v.name), field("suite", v.suite), field("operation", v.operation), field("cite", v.cite), field("input", v.input))
	if v.negativeOf != "" {
		o = append(o, field("negative_of", v.negativeOf), field("mutation", v.mutation))
	}
	o = append(o, field("expect", v.expect))
	return jsonBytes(o)
}

var cases []caseVector

func accept(name, suite, operation, cite string, in, out ordered) {
	cases = append(cases, caseVector{name: name, suite: suite, operation: operation, cite: cite, input: in, expect: obj(field("verdict", "accept"), field("outputs", out))})
}

type rejection struct {
	parent, kind, path, semantic, stage, reason string
}

func reject(name, suite, operation, cite string, in ordered, r rejection) {
	cases = append(cases, caseVector{name: name, suite: suite, operation: operation, cite: cite, input: in, negativeOf: r.parent, mutation: obj(field("kind", r.kind), field("path", r.path), field("semantic_field", r.semantic)), expect: obj(field("verdict", "reject"), field("stage", r.stage), field("reason", r.reason))})
}
func replace(o ordered, key string, value any) ordered {
	n := append(ordered(nil), o...)
	for i := range n {
		if n[i].key == key {
			n[i].value = value
			return n
		}
	}
	panic("missing field " + key)
}

func context(tag string, fields []any) []byte {
	a := append([]any{"concord-e2ee-context", 1, tag}, fields...)
	return jsonBytes(a)
}
func contextInput(tag string, fields []any) ordered {
	return obj(field("tag", tag), field("context_fields", fields))
}
func contextParseInput(tag string, fields []any) ordered {
	return obj(field("context_b64", b64(context(tag, fields))))
}
func contextCases(tag string, fields []any) {
	cite := contextCitation
	accept(contextEncodePrefix+tag+"/base", "context", "encode", cite, contextInput(tag, fields), obj(field("context_b64", b64(context(tag, fields)))))
	accept("context/parse/"+tag+"/base", "context", "parse", cite, contextParseInput(tag, fields), obj(field("tag", tag), field("context_fields", fields)))
}
func seal(key, nonce, aad, plain []byte) []byte {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, len(nonce))
	if err != nil {
		panic(err)
	}
	return gcm.Seal(nil, nonce, plain, aad)
}
func envelope(format string, nonce, sealed, fileNonce []byte) []byte {
	if format == attachmentV4 {
		h := make([]byte, 28, 28+len(nonce)+len(sealed))
		copy(h, []byte("CV"))
		h[2] = 4
		binary.BigEndian.PutUint32(h[4:8], 8388608)
		binary.BigEndian.PutUint32(h[8:12], 1)
		copy(h[12:], fileNonce)
		return append(append(h, nonce...), sealed...)
	}
	prefix := map[string]string{"m1": "m1:", "b1": "b1:", "ek1": "ek1:", "rw1-private": "rw1:", "rw1-preferences": "rw1:"}[format]
	return []byte(prefix + b64(append(append([]byte{}, nonce...), sealed...)))
}
func credential(password []byte, realm string, subject []byte, generation uint64, salt []byte) (root, auth, wrap, prefs []byte) {
	root = argon2.IDKey(password, salt, 3, 65536, 4, 32)
	bind := append([]byte("concord-credential-bind-v1\x00"+realm), subject...)
	var gen [8]byte
	binary.BigEndian.PutUint64(gen[:], generation)
	bind = append(bind, gen[:]...)
	prk := hkdf.Extract(sha256.New, root, bind)
	expand := func(label string) []byte {
		out := make([]byte, 32)
		r := hkdf.Expand(sha256.New, prk, []byte(label+"\x00"))
		if _, err := r.Read(out); err != nil {
			panic(err)
		}
		return out
	}
	auth = expand("concord-auth-secret-v1")
	wrap = expand("concord-wrap-key-v1")
	prefs = expand("concord-prefs-key-v1")
	return
}
func descriptor(realm string, subject []byte, generation uint64, salt []byte, m, t, p int) []byte {
	return jsonBytes([]any{splitAuthWrapV1, realm, b64(subject), generation, b64(salt), m, t, p})
}
func credentialCases() {
	cite := domainSeparationCitation
	accept("credential/password-scalars/base", "credential", passwordScalars, cite, obj(field("utf16_units_hex", "007000e100730073")), obj(field("password_utf8_b64", b64([]byte("páss")))))
	accept("credential/password-scalars/empty", "credential", passwordScalars, cite, obj(field("utf16_units_hex", "")), obj(field("password_utf8_b64", "")))
	reject("credential/password-scalars/invalid-surrogate", "credential", passwordScalars, cite, obj(field("utf16_units_hex", "007000e100730073d800")), rejection{"credential/password-scalars/base", "insert", "/input/utf16_units_hex", "utf16_units_hex", "validate", "invalid_unicode"})
	subject, salt := fixed(32, 0x30), fixed(16, 0x50)
	for _, g := range []struct {
		name       string
		generation uint64
	}{{"base", 1}, {"max-generation", 9007199254740991}} {
		root, auth, wrap, prefs := credential([]byte("páss"), realm, subject, g.generation, salt)
		accept("credential/derive/"+g.name, "credential", "derive", cite, obj(field("password_utf8_b64", b64([]byte("páss"))), field("realm", realm), field("subject_b64", b64(subject)), field("generation", g.generation), field("salt_b64", b64(salt))), obj(field("root_b64", b64(root)), field("auth_b64", base64.RawURLEncoding.EncodeToString(auth)), field("wrap_b64", b64(wrap)), field("prefs_b64", b64(prefs))))
	}
	deriveBase := obj(field("password_utf8_b64", b64([]byte("páss"))), field("realm", realm), field("subject_b64", b64(subject)), field("generation", 1), field("salt_b64", b64(salt)))
	for _, bad := range []struct {
		name, key string
		value     any
	}{
		{"short-subject", "subject_b64", b64(subject[:31])},
		{"short-salt", "salt_b64", b64(salt[:15])},
		{"invalid-password-utf8", "password_utf8_b64", b64([]byte{0xff})},
	} {
		reject("credential/derive/"+bad.name, "credential", "derive", cite, replace(deriveBase, bad.key, bad.value), rejection{"credential/derive/base", "replace", "/input/" + bad.key, bad.key, "validate", "invalid_field"})
	}
	base := obj(field("authenticated_realm", realm), field("descriptor_b64", b64(descriptor(realm, subject, 1, salt, 65536, 3, 4))))
	out := obj(field("realm", realm), field("subject_b64", b64(subject)), field("generation", 1), field("salt_b64", b64(salt)), field("m_kib", 65536), field("t", 3), field("p", 4))
	accept(descriptorBase, "credential", "descriptor", cite, base, out)
	reject("credential/descriptor/weak-parameters", "credential", "descriptor", cite, replace(base, "descriptor_b64", b64(descriptor(realm, subject, 1, salt, 32768, 3, 4))), rejection{descriptorBase, "replace", descriptorInputPath, "m_kib", "validate", "weak_parameters"})
	reject("credential/descriptor/wrong-realm", "credential", "descriptor", cite, replace(base, "authenticated_realm", otherRealm), rejection{descriptorBase, "replace", "/input/authenticated_realm", "authenticated_realm", "validate", "wrong_realm"})
	withWhitespace := append([]byte(" "), descriptor(realm, subject, 1, salt, 65536, 3, 4)...)
	reject("credential/descriptor/noncanonical-whitespace", "credential", "descriptor", cite, replace(base, "descriptor_b64", b64(withWhitespace)), rejection{descriptorBase, "insert", descriptorInputPath, "descriptor_b64", "parse", "malformed_descriptor"})
	badSubject := jsonBytes([]any{splitAuthWrapV1, realm, "not-base64", 1, b64(salt), 65536, 3, 4})
	reject("credential/descriptor/invalid-subject-base64", "credential", "descriptor", cite, replace(base, "descriptor_b64", b64(badSubject)), rejection{descriptorBase, "replace", descriptorInputPath, "subject_b64", "parse", "malformed_descriptor"})
	wrongProtocol := jsonBytes([]any{serverSelectedV2, realm, b64(subject), 1, b64(salt), 65536, 3, 4})
	reject("credential/descriptor/wrong-discriminator", "credential", "descriptor", cite, replace(base, "descriptor_b64", b64(wrongProtocol)), rejection{descriptorBase, "replace", descriptorInputPath, "protocol", "parse", "malformed_descriptor"})
	reject("credential/descriptor/zero-generation", "credential", "descriptor", cite, replace(base, "descriptor_b64", b64(descriptor(realm, subject, 0, salt, 65536, 3, 4))), rejection{descriptorBase, "replace", descriptorInputPath, "generation", "parse", "malformed_descriptor"})
	trailingField := jsonBytes([]any{splitAuthWrapV1, realm, b64(subject), 1, b64(salt), 65536, 3, 4, "extra"})
	reject("credential/descriptor/trailing-field", "credential", "descriptor", cite, replace(base, "descriptor_b64", b64(trailingField)), rejection{descriptorBase, "insert", descriptorInputPath, "descriptor_b64", "parse", "malformed_descriptor"})
}

type tagFixture struct {
	tag, format string
	names       []string
	fields      []any
}

func fixtures() []tagFixture {
	fileNonce := b64(fixed(16, 0x70))
	digest := b64(fixed(48, 0x90))
	return []tagFixture{
		{"message-v1", "m1", []string{"realm", "scope", "scope_id", "message_id", "sender_id", "key_epoch", "content_kind"}, []any{realm, "channel", objectID, otherObject, actor, 1, "message-body"}},
		{"blob-v1", "b1", []string{"realm", "account_id", "blob_purpose", "schema_version"}, []any{realm, account, "preferences", 1}},
		{"account-ek-wrap-v1", "ek1", []string{"realm", "account_id", "key_purpose", "account_key_generation", "credential_generation"}, []any{realm, account, accountKeyPurpose, 1, 1}},
		{"recovery-wrap-v1", "rw1-private", []string{"realm", "account_id", "key_purpose", "recovery_purpose", "recovery_key_generation", "account_key_generation"}, []any{realm, account, accountKeyPurpose, "user-account-recovery-key", 1, 1}},
		{recoveryPrefsWrapV1, "rw1-preferences", []string{"realm", "account_id", "key_purpose", "recovery_purpose", "recovery_key_generation", "credential_generation"}, []any{realm, account, preferencesPurpose, "user-account-recovery-key", 1, 1}},
		{attachmentV4, attachmentV4, []string{"realm", "scope", "scope_id", "file_id", "uploader_id", "key_epoch", "file_nonce_b64", "flags", "chunk_index", "total_chunks", "chunk_size"}, []any{realm, "channel", objectID, otherObject, actor, 1, fileNonce, 0, 0, 1, 8388608}},
		{"channel-key-capsule-v1", "", []string{"realm", "scope", "scope_id", "key_epoch", "recipient_id", "recipient_key_generation", "distributor_id", "distributor_root_digest_b64"}, []any{realm, "channel", objectID, 1, account, 1, actor, digest}},
	}
}
func alteredField(name string, old any) (any, bool) {
	switch name {
	case "realm":
		return otherRealm, false
	case "account_id", "recipient_id":
		return otherAccount, false
	case "scope_id", "message_id", "file_id":
		if old == otherObject {
			return objectID, false
		}
		return otherObject, false
	case "sender_id", "uploader_id", "distributor_id":
		return otherActor, false
	case "scope":
		return "dm", false
	case "blob_purpose":
		return "saved_gifs", false
	case "file_nonce_b64":
		return b64(fixed(16, 0x71)), false
	case "distributor_root_digest_b64":
		return b64(fixed(48, 0x91)), false
	case "content_kind", "key_purpose", "recovery_purpose":
		return "invalid-v1-purpose", true
	case "schema_version", "flags", "chunk_index", "total_chunks", "chunk_size":
		return 2, true
	default:
		if n, ok := old.(int); ok {
			return n + 1, false
		}
		panic("unhandled field " + name)
	}
}
func alteredFields(f tagFixture, index int, value any) []any {
	n := append([]any(nil), f.fields...)
	n[index] = value
	return n
}
func fixtureFileNonce(f tagFixture) []byte {
	if f.tag != attachmentV4 {
		return nil
	}
	return must(base64.StdEncoding.DecodeString(f.fields[6].(string)))
}
func aeadInput(format string, key, nonce, ctx, plain []byte) ordered {
	return obj(field("format", format), field("key_b64", b64(key)), field("nonce_b64", b64(nonce)), field("context_b64", b64(ctx)), field("plaintext_b64", b64(plain)))
}
func decryptInput(format string, key, ctx, envelope []byte) ordered {
	return obj(field("format", format), field("key_b64", b64(key)), field("expected_context_b64", b64(ctx)), field("envelope_b64", b64(envelope)))
}
func aeadCases(f tagFixture) {
	cite := "ADR-0051 Canonical non-authority context and AES-GCM"
	key, nonce, plain := fixed(32, 0x10), fixed(12, 0x40), []byte("public test plaintext")
	ctx := context(f.tag, f.fields)
	env := envelope(f.format, nonce, seal(key, nonce, ctx, plain), fixtureFileNonce(f))
	encName := aeadEncryptPrefix + f.tag + "/base"
	decName := aeadDecryptPrefix + f.tag + "/base"
	enc := aeadInput(f.format, key, nonce, ctx, plain)
	dec := decryptInput(f.format, key, ctx, env)
	accept(encName, "aead", "encrypt", cite, enc, obj(field("envelope_b64", b64(env))))
	accept(decName, "aead", "decrypt", cite, dec, obj(field("plaintext_b64", b64(plain))))
	for i, name := range f.names {
		alt, invalid := alteredField(name, f.fields[i])
		altered := context(f.tag, alteredFields(f, i, alt))
		stage, reason := "authenticate", "aead_failure"
		if invalid {
			stage, reason = "validate", "invalid_field"
		}
		reject(aeadDecryptPrefix+f.tag+"/alter-"+name, "aead", "decrypt", cite, replace(dec, "expected_context_b64", b64(altered)), rejection{decName, "replace", "/input/expected_context_b64", name, stage, reason})
	}
	for _, bad := range []struct {
		name   string
		length int
		key    bool
	}{{"short-key", 31, true}, {"long-key", 33, true}, {shortNonce, 8, false}, {"long-nonce", 16, false}} {
		if bad.key {
			k := fixed(bad.length, 0x10)
			reject(aeadEncryptPrefix+f.tag+"/"+bad.name, "aead", "encrypt", cite, replace(enc, "key_b64", b64(k)), rejection{encName, "replace", "/input/key_b64", "key_b64", "validate", "invalid_field"})
			reject(aeadDecryptPrefix+f.tag+"/"+bad.name, "aead", "decrypt", cite, replace(dec, "key_b64", b64(k)), rejection{decName, "replace", "/input/key_b64", "key_b64", "validate", "invalid_field"})
			continue
		}
		n := fixed(bad.length, 0x40)
		reject(aeadEncryptPrefix+f.tag+"/"+bad.name, "aead", "encrypt", cite, replace(enc, "nonce_b64", b64(n)), rejection{encName, "replace", "/input/nonce_b64", "nonce_b64", "validate", "invalid_field"})
		p := plain
		if bad.name == shortNonce {
			p = nil
		}
		badEnv := envelope(f.format, n, seal(key, n, ctx, p), fixtureFileNonce(f))
		stage, reason := "authenticate", "aead_failure"
		if bad.name == shortNonce {
			stage, reason = "parse", "bad_envelope"
		}
		reject(aeadDecryptPrefix+f.tag+"/"+bad.name, "aead", "decrypt", cite, replace(dec, "envelope_b64", b64(badEnv)), rejection{decName, "replace", envelopeInputPath, "nonce_b64", stage, reason})
	}
	otherFormat := "m1"
	if f.format == "m1" {
		otherFormat = "b1"
	}
	reject(aeadDecryptPrefix+f.tag+"/wrong-format", "aead", "decrypt", cite, replace(dec, "format", otherFormat), rejection{decName, "replace", "/input/format", "format", "validate", "invalid_format_context"})
	if f.tag == attachmentV4 {
		bad := append([]byte(nil), env...)
		bad[12] ^= 1
		reject("aead/decrypt/attachment-v4/tamper-header", "aead", "decrypt", cite, replace(dec, "envelope_b64", b64(bad)), rejection{decName, "replace", envelopeInputPath, "file_nonce_b64", "authenticate", "aead_failure"})
		for _, wireChange := range []struct {
			name, semantic, stage, reason string
			change                        func([]byte)
		}{
			{"tamper-flags", "flags", "validate", "invalid_field", func(b []byte) { b[3] = 1 }},
			{"tamper-chunk-size", "chunk_size", "validate", "invalid_field", func(b []byte) { binary.BigEndian.PutUint32(b[4:8], 1) }},
			{"tamper-total-chunks", "total_chunks", "validate", "invalid_field", func(b []byte) { binary.BigEndian.PutUint32(b[8:12], 2) }},
			{"tamper-version", "version", "parse", "bad_envelope", func(b []byte) { b[2] = 3 }},
			{"tamper-magic", "magic", "parse", "bad_envelope", func(b []byte) { b[0] = 'X' }},
		} {
			changed := append([]byte(nil), env...)
			wireChange.change(changed)
			reject("aead/decrypt/attachment-v4/"+wireChange.name, "aead", "decrypt", cite, replace(dec, "envelope_b64", b64(changed)), rejection{decName, "replace", envelopeInputPath, wireChange.semantic, wireChange.stage, wireChange.reason})
		}
	}
}
func rsaCases(f tagFixture) {
	cite := "ADR-0051 Channel-key distribution"
	base := "rsa-label/verify/" + f.tag + "/base"
	label := context(f.tag, f.fields)
	accept(rsaLabelDerivePrefix+f.tag+"/base", rsaLabelSuite, "derive", cite, obj(field("context_fields", f.fields)), obj(field("label_b64", b64(label))))
	reject(rsaLabelDerivePrefix+f.tag+"/invalid-recipient-id", rsaLabelSuite, "derive", cite, obj(field("context_fields", alteredFields(f, 4, invalidUUID))), rejection{rsaLabelDerivePrefix + f.tag + "/base", "replace", "/input/context_fields/4", "recipient_id", "validate", "invalid_field"})
	input := obj(field("context_fields", f.fields), field("candidate_label_b64", b64(label)))
	accept(base, rsaLabelSuite, "verify", cite, input, obj())
	for i, name := range f.names {
		alt, invalid := alteredField(name, f.fields[i])
		stage, reason := "authenticate", "invalid_label"
		if invalid {
			stage, reason = "validate", "invalid_field"
		}
		reject("rsa-label/verify/"+f.tag+"/alter-"+name, rsaLabelSuite, "verify", cite, replace(input, "context_fields", alteredFields(f, i, alt)), rejection{base, "replace", fmt.Sprintf("/input/context_fields/%d", i), name, stage, reason})
	}
}
func allContextCases() {
	for _, f := range fixtures() {
		contextCases(f.tag, f.fields)
		if f.tag == "channel-key-capsule-v1" {
			rsaCases(f)
		} else {
			aeadCases(f)
		}
	}
	for _, purpose := range []string{"preferences", "saved_gifs", "friend_organization", "presence_overrides"} {
		f := fixtures()[1]
		f.fields[2] = purpose
		accept("context/encode/blob-v1/"+purpose, "context", "encode", contextCitation, contextInput(f.tag, f.fields), obj(field("context_b64", b64(context(f.tag, f.fields)))))
	}
	for _, tag := range []string{"account-ek-wrap-v1", "recovery-wrap-v1", recoveryPrefsWrapV1} {
		var f tagFixture
		for _, candidate := range fixtures() {
			if candidate.tag == tag {
				f = candidate
				break
			}
		}
		wrong := preferencesPurpose
		if tag == recoveryPrefsWrapV1 {
			wrong = accountKeyPurpose
		}
		bad := alteredFields(f, 2, wrong)
		reject(contextEncodePrefix+tag+"/wrong-key-purpose", "context", "encode", contextCitation, contextInput(tag, bad), rejection{contextEncodePrefix + tag + "/base", "replace", "/input/context_fields/2", "key_purpose", "validate", "invalid_purpose"})
	}
	cite := contextCitation
	message := fixtures()[0]
	reject("context/encode/message-v1/unknown-tag", "context", "encode", cite, contextInput(serverSelectedV2, message.fields), rejection{"context/encode/message-v1/base", "replace", "/input/tag", "tag", "validate", "unknown_tag"})
	reject("context/encode/message-v1/invalid-realm", "context", "encode", cite, contextInput(message.tag, alteredFields(message, 0, invalidUUID)), rejection{"context/encode/message-v1/base", "replace", "/input/context_fields/0", "realm", "validate", "invalid_field"})
	reject("context/parse/message-v1/unknown-tag", "context", "parse", cite, obj(field("context_b64", b64(context(serverSelectedV2, message.fields)))), rejection{messageParseBase, "replace", contextInputPath, "tag", "validate", "unknown_tag"})
	withWhitespace := bytes.Replace(context(message.tag, message.fields), []byte(","), []byte(", "), 1)
	reject("context/parse/message-v1/noncanonical-whitespace", "context", "parse", cite, obj(field("context_b64", b64(withWhitespace))), rejection{messageParseBase, "insert", contextInputPath, "context_b64", "parse", "malformed_context"})
	reject("context/parse/message-v1/invalid-realm", "context", "parse", cite, contextParseInput(message.tag, alteredFields(message, 0, invalidUUID)), rejection{messageParseBase, "replace", contextInputPath, "realm", "validate", "invalid_field"})
	accountWrap := fixtures()[2]
	reject("context/parse/account-ek-wrap-v1/wrong-key-purpose", "context", "parse", cite, contextParseInput(accountWrap.tag, alteredFields(accountWrap, 2, preferencesPurpose)), rejection{"context/parse/account-ek-wrap-v1/base", "replace", contextInputPath, "key_purpose", "validate", "invalid_purpose"})
	attachment := fixtures()[5]
	reject("context/parse/attachment-v4/invalid-flags", "context", "parse", cite, contextParseInput(attachment.tag, alteredFields(attachment, 7, 1)), rejection{attachmentParseBase, "replace", contextInputPath, "flags", "validate", "invalid_field"})
	reject("context/parse/attachment-v4/invalid-chunk-index", "context", "parse", cite, contextParseInput(attachment.tag, alteredFields(attachment, 8, 2)), rejection{attachmentParseBase, "replace", contextInputPath, "chunk_index", "validate", "invalid_field"})
	reject("context/parse/attachment-v4/fractional-chunk-index", "context", "parse", cite, contextParseInput(attachment.tag, alteredFields(attachment, 8, 0.5)), rejection{attachmentParseBase, "replace", contextInputPath, "chunk_index", "validate", "invalid_field"})
	reject("context/parse/attachment-v4/fractional-total-chunks", "context", "parse", cite, contextParseInput(attachment.tag, alteredFields(attachment, 9, 0.5)), rejection{attachmentParseBase, "replace", contextInputPath, "total_chunks", "validate", "invalid_field"})
}

func uniformityCases() {
	cite := domainSeparationCitation
	for i, class := range []string{"split", "legacy", "unknown"} {
		// All descriptor bytes are public synthetic fixtures; none includes the probe identity.
		subject, salt := fixed(32, byte(0xa0+i)), fixed(16, byte(0xc0+i))
		in := obj(field("account_class", class), field("probe_public_account_uuid", otherObject), field("descriptor_b64", b64(descriptor(realm, subject, 1, salt, 65536, 3, 4))), field("proof_verdict", "wrong"))
		out := obj(field("descriptor_shape", "split-auth-wrap-v1-fixed"), field("failed_proof_status", 401), field("failed_proof_shape", "opaque-continuation-v1"), field("failed_proof_error_class", "invalid-credentials"), field("attempt_charge", 1))
		accept("policy/descriptor-uniformity/"+class, "policy", "descriptor-uniformity", cite, in, out)
	}
}

type legacyDecisionInput struct {
	device, protocol, proof  string
	realmMatch, accountMatch bool
	freshness                string
	notExpired, continuation bool
}

func legacyInput(p legacyDecisionInput) ordered {
	return obj(field("device_state", p.device), field("account_protocol", p.protocol), field("authority_proof", p.proof), field("realm_match", p.realmMatch), field("account_match", p.accountMatch), field("freshness", p.freshness), field("not_expired", p.notExpired), field("control_plane_continuation", p.continuation))
}
func generatedLegacyDecision(protocol, proof string, realmMatch, accountMatch bool, freshness string, notExpired, continuation bool) (string, string) {
	switch {
	case protocol != "legacy":
		return "deny", "not-legacy"
	case proof == "absent":
		return "deny", "missing-authority-proof"
	case proof == "invalid":
		return "deny", "invalid-authority-proof"
	case freshness != "current":
		return "deny", "stale-authority-proof"
	case !accountMatch:
		return "deny", "wrong-account"
	case !realmMatch:
		return "deny", "wrong-realm"
	case !notExpired:
		return "deny", "expired-proof"
	case !continuation:
		return "deny", "no-continuation"
	default:
		return "submit-raw-password", "eligible"
	}
}
func policyOutput(decision, reason string) ordered {
	return obj(field("decision", decision), field("reason", reason))
}
func slot(b bool) string {
	if b {
		return "t"
	}
	return "f"
}
func addLegacy(name string, in ordered) {
	p := func(key string) any {
		for _, e := range in {
			if e.key == key {
				return e.value
			}
		}
		panic(key)
	}
	decision, reason := generatedLegacyDecision(p("account_protocol").(string), p("authority_proof").(string), p("realm_match").(bool), p("account_match").(bool), p("freshness").(string), p("not_expired").(bool), p("control_plane_continuation").(bool))
	accept(name, "policy", "legacy-continuation", "ADR-0051 Credential domain separation and Migration and activation", in, policyOutput(decision, reason))
}
func combinations(dimensions [][]any, visit func([]any)) {
	values := make([]any, len(dimensions))
	var walk func(int)
	walk = func(index int) {
		if index == len(dimensions) {
			visit(values)
			return
		}
		for _, value := range dimensions[index] {
			values[index] = value
			walk(index + 1)
		}
	}
	walk(0)
}
func legacyCases() {
	base := legacyInput(legacyDecisionInput{"existing", "legacy", "verified", true, true, "current", true, true})
	for _, named := range []struct {
		name    string
		changes []entry
	}{
		{"eligible", nil},
		{"forged-split", []entry{field("account_protocol", "split")}},
		{"invalid-proof", []entry{field("authority_proof", "invalid")}},
		{"existing-device-no-proof", []entry{field("authority_proof", "absent")}},
		{"stale-post-upgrade", []entry{field("freshness", "stale")}},
		{"wrong-account", []entry{field("account_match", false)}},
		{"wrong-realm", []entry{field("realm_match", false)}},
		{"expired-proof", []entry{field("not_expired", false)}},
		{"no-continuation", []entry{field("control_plane_continuation", false)}},
		{"fresh-device-no-proof", []entry{field("device_state", "fresh"), field("authority_proof", "absent")}},
		{"fresh-device-invalid-proof", []entry{field("device_state", "fresh"), field("authority_proof", "invalid")}},
	} {
		in := base
		for _, change := range named.changes {
			in = replace(in, change.key, change.value)
		}
		addLegacy("policy/legacy-continuation/"+named.name, in)
	}
	combinations([][]any{
		{"existing", "fresh"}, {"legacy", "split"}, {"verified", "absent", "invalid"},
		{true, false}, {true, false}, {"current", "stale"}, {true, false}, {true, false},
	}, func(v []any) {
		device, protocol, proof := v[0].(string), v[1].(string), v[2].(string)
		realmMatch, accountMatch := v[3].(bool), v[4].(bool)
		freshness := v[5].(string)
		notExpired, continuation := v[6].(bool), v[7].(bool)
		name := fmt.Sprintf("policy/legacy-continuation/matrix/%s/%s/%s/%s/%s/%s/%s/%s", device, protocol, proof, slot(realmMatch), slot(accountMatch), freshness, slot(notExpired), slot(continuation))
		addLegacy(name, legacyInput(legacyDecisionInput{device, protocol, proof, realmMatch, accountMatch, freshness, notExpired, continuation}))
	})
}
func upgradeInput(prior, authorization string, match, mfa bool, commit string) ordered {
	return obj(field("prior_state", prior), field("authorization_state", authorization), field("request_match", match), field("mfa_complete", mfa), field("commit_state", commit))
}
func generatedUpgradeDecision(prior, authorization string, match, mfa bool, commit string) (string, string) {
	switch {
	case commit == "ambiguous":
		return "reconcile", ambiguousCommit
	case !mfa:
		return "deny", "mfa-required"
	case commit == "committed":
		if prior == "split" && authorization == "consumed" {
			if match {
				return "idempotent", "replay"
			}
			return "conflict", requestConflict
		}
		return "reconcile", ambiguousCommit
	case prior == "split":
		return "deny", "not-legacy"
	case authorization == "consumed":
		return "reconcile", ambiguousCommit
	case authorization == "expired":
		return "deny", "expired-authorization"
	case authorization == "absent":
		return "deny", "missing-authorization"
	case !match:
		return "conflict", requestConflict
	default:
		return "commit", "fresh-authorization"
	}
}
func addUpgrade(name string, in ordered) {
	p := func(key string) any {
		for _, e := range in {
			if e.key == key {
				return e.value
			}
		}
		panic(key)
	}
	decision, reason := generatedUpgradeDecision(p("prior_state").(string), p("authorization_state").(string), p("request_match").(bool), p("mfa_complete").(bool), p("commit_state").(string))
	accept(name, "policy", "upgrade-outcome", domainSeparationCitation, in, policyOutput(decision, reason))
}
func upgradeCases() {
	base := upgradeInput("legacy", "fresh", true, true, "none")
	for _, named := range []struct {
		name    string
		changes []entry
	}{
		{"fresh-authorization", nil},
		{"fresh-request-conflict", []entry{field("request_match", false)}},
		{"already-split", []entry{field("prior_state", "split")}},
		{"replay", []entry{field("prior_state", "split"), field("authorization_state", "consumed"), field("commit_state", "committed")}},
		{requestConflict, []entry{field("prior_state", "split"), field("authorization_state", "consumed"), field("request_match", false), field("commit_state", "committed")}},
		{"mfa-required", []entry{field("mfa_complete", false)}},
		{ambiguousCommit, []entry{field("commit_state", "ambiguous")}},
		{"expired-authorization", []entry{field("authorization_state", "expired")}},
		{"missing-authorization", []entry{field("authorization_state", "absent")}},
	} {
		in := base
		for _, change := range named.changes {
			in = replace(in, change.key, change.value)
		}
		addUpgrade("policy/upgrade-outcome/"+named.name, in)
	}
	combinations([][]any{
		{"legacy", "split"}, {"fresh", "consumed", "expired", "absent"},
		{true, false}, {true, false}, {"none", "committed", "ambiguous"},
	}, func(v []any) {
		prior, authorization := v[0].(string), v[1].(string)
		match, mfa, commit := v[2].(bool), v[3].(bool), v[4].(string)
		name := fmt.Sprintf("policy/upgrade-outcome/matrix/%s/%s/%s/%s/%s", prior, authorization, slot(match), slot(mfa), commit)
		addUpgrade(name, upgradeInput(prior, authorization, match, mfa, commit))
	})
}
func generate(source []byte) []byte {
	cases = nil
	credentialCases()
	allContextCases()
	uniformityCases()
	legacyCases()
	upgradeCases()
	var body bytes.Buffer
	for i, v := range cases {
		if i != 0 {
			body.WriteString(",\n")
		}
		body.Write(v.bytes())
	}
	body.WriteString("\n]}\n")
	header := fmt.Sprintf("{\"schema\":\"concord-nonmedia-e2ee-vectors\",\"version\":1,\"generator_sha384\":\"%s\",\"vectors_sha384\":\"%s\",\"vectors\":[\n", sha384Of(source), sha384Of(body.Bytes()))
	return append([]byte(header), body.Bytes()...)
}
func root() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, adrPath)); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("repository root not found")
		}
		dir = parent
	}
}
func run() error {
	if len(os.Args) != 2 || (os.Args[1] != "--write" && os.Args[1] != "--check") {
		return errors.New("usage: go run ./cmd/nonmedia-vectors --write|--check")
	}
	r, err := root()
	if err != nil {
		return err
	}
	// #nosec G304 -- generatorPath is a compile-time repository-relative constant.
	source, err := os.ReadFile(filepath.Join(r, generatorPath))
	if err != nil {
		return err
	}
	generated := generate(source)
	if len(generated) > 1<<20 {
		return fmt.Errorf("corpus exceeds 1 MiB: %d bytes", len(generated))
	}
	path := filepath.Join(r, corpusPath)
	if os.Args[1] == "--write" {
		if err := os.WriteFile(path, generated, 0600); err != nil {
			return err
		}
		fmt.Printf("corpus SHA-384: %s (%d vectors, %d bytes)\n", sha384Of(generated), len(cases), len(generated))
		return nil
	}
	// #nosec G304 -- corpusPath is a compile-time repository-relative constant.
	committed, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(committed, generated) {
		return errors.New("committed corpus differs from deterministic generation")
	}
	// #nosec G304 -- adrPath is a compile-time repository-relative constant.
	adr, err := os.ReadFile(filepath.Join(r, adrPath))
	if err != nil {
		return err
	}
	match := regexp.MustCompile("(?m)^\\*\\*Corpus SHA-384:\\*\\* `([0-9a-f]{96})`$").FindSubmatch(adr)
	if len(match) != 2 || string(match[1]) != sha384Of(generated) {
		return errors.New("ADR whole-file SHA-384 pin missing or mismatched")
	}
	return nil
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
