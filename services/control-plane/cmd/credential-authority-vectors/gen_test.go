package main

import (
	"bytes"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	credential "github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/authoritycredential"
)

func TestGeneratorCheckModeMatchesCommittedCorpus(t *testing.T) {
	root, err := repositoryRoot()
	require.NoError(t, err)
	generated, err := generate(root)
	require.NoError(t, err)
	committed, err := os.ReadFile(filepath.Join(root, corpusPath)) // #nosec G304 -- corpusPath is a repository constant.
	require.NoError(t, err)
	require.True(t, bytes.Equal(generated, committed))
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	os.Args = []string{"credential-authority-vectors", "--check"}
	require.NoError(t, run())
	os.Args = []string{"credential-authority-vectors", "--unexpected"}
	require.Error(t, run())
}

func TestGeneratorFailsClosedOutsideARepositoryAndWritesOnlyItsCorpus(t *testing.T) {
	oldArgs := os.Args
	oldDirectory, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() {
		os.Args = oldArgs
		require.NoError(t, os.Chdir(oldDirectory))
	})

	noRoot := t.TempDir()
	require.NoError(t, os.Chdir(noRoot))
	_, err = repositoryRoot()
	require.ErrorContains(t, err, "repository root not found")
	os.Args = []string{"credential-authority-vectors", "--check", "extra"}
	require.ErrorContains(t, run(), "usage")

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "CHANGELOG.md"), nil, 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, filepath.Dir(generatorPath)), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, generatorPath), []byte("generator"), 0o600))
	require.NoError(t, os.Chdir(root))
	os.Args = []string{"credential-authority-vectors", "--check"}
	require.ErrorContains(t, run(), "read corpus")
	require.NoError(t, os.MkdirAll(filepath.Join(root, filepath.Dir(corpusPath)), 0o700))
	os.Args = []string{"credential-authority-vectors", "--write"}
	require.NoError(t, run())
	os.Args = []string{"credential-authority-vectors", "--check"}
	require.NoError(t, run())
}

func TestCorpusVectorsMatchCodec(t *testing.T) {
	root, err := repositoryRoot()
	require.NoError(t, err)
	raw, err := os.ReadFile(filepath.Join(root, corpusPath)) // #nosec G304 -- corpusPath is a repository constant.
	require.NoError(t, err)
	var parsed corpus
	require.NoError(t, json.Unmarshal(raw, &parsed))
	for _, item := range parsed.Vectors {
		t.Run(item.Name, func(t *testing.T) {
			encoded, ok := item.Input["raw_b64"].(string)
			require.True(t, ok)
			wire, err := base64.StdEncoding.Strict().DecodeString(encoded)
			require.NoError(t, err)
			var decodeErr error
			switch item.Suite {
			case "publication":
				_, decodeErr = credential.DecodePublication(wire)
			case "evidence":
				_, decodeErr = credential.DecodeEvidenceCarrier(wire)
			case "status":
				_, decodeErr = credential.DecodeLegacyStatus(wire)
			case "envelope":
				var envelope credential.SignedCredential
				envelope, decodeErr = credential.DecodeSignedEnvelope(wire)
				if decodeErr == nil && item.Operation == "verify" {
					pointText, ok := item.Input["point_b64"].(string)
					require.True(t, ok)
					point, err := base64.StdEncoding.Strict().DecodeString(pointText)
					require.NoError(t, err)
					require.True(t, credential.VerifyP384Signature(point, envelope.Inner, envelope.Signature[:]))
				}
			default:
				t.Fatalf("unexpected suite %q", item.Suite)
			}
			verdict, ok := item.Expect["verdict"].(string)
			require.True(t, ok)
			if verdict == "accept" {
				require.NoError(t, decodeErr)
				return
			}
			require.Equal(t, "reject", verdict)
			require.Error(t, decodeErr)
		})
	}
}

func TestCorpusPinsAndClosedVectorShapes(t *testing.T) {
	root, err := repositoryRoot()
	require.NoError(t, err)
	raw, err := os.ReadFile(filepath.Join(root, corpusPath)) // #nosec G304 -- corpusPath is a repository constant.
	require.NoError(t, err)
	require.LessOrEqual(t, len(raw), 1<<20)
	require.True(t, bytes.HasSuffix(raw, []byte("\n")))
	for _, value := range raw {
		require.LessOrEqual(t, value, byte(0x7f))
	}
	var parsed corpus
	require.NoError(t, json.Unmarshal(raw, &parsed))
	require.Equal(t, "concord-credential-authority-v2-vectors", parsed.Schema)
	require.Equal(t, 1, parsed.Version)
	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{96}$`), parsed.GeneratorSHA384)
	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{96}$`), parsed.VectorsSHA384)
	source, err := os.ReadFile(filepath.Join(root, generatorPath)) // #nosec G304 -- generatorPath is a repository constant.
	require.NoError(t, err)
	digest := sha512.Sum384(source)
	require.Equal(t, parsed.GeneratorSHA384, hex.EncodeToString(digest[:]))
	vectors, err := json.Marshal(parsed.Vectors)
	require.NoError(t, err)
	digest = sha512.Sum384(vectors)
	require.Equal(t, parsed.VectorsSHA384, hex.EncodeToString(digest[:]))
	seen := map[string]struct{}{}
	for _, vector := range parsed.Vectors {
		require.Regexp(t, regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,127}$`), vector.Name)
		_, exists := seen[vector.Name]
		require.False(t, exists, "duplicate vector %q", vector.Name)
		seen[vector.Name] = struct{}{}
		require.NotEmpty(t, vector.Suite)
		require.NotEmpty(t, vector.Operation)
		require.NotEmpty(t, vector.Input)
		require.NotEmpty(t, vector.Expect)
	}
}
