// Test fixture for concord-crypto rules
// This file should trigger Semgrep findings — it is NOT production code.

// ruleid: concord-crypto-outside-module
// crypto.subtle used outside designated modules
const key = await crypto.subtle.generateKey(
  { name: 'AES-GCM', length: 256 },
  true,
  ['encrypt', 'decrypt']
);

// ruleid: concord-crypto-deprecated-md5
const hash = createHash("md5");

// ruleid: concord-crypto-deprecated-des
const cipher = "DES-CBC";
const streamCipher = "RC4";
