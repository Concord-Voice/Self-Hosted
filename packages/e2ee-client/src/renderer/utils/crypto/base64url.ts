/**
 * base64url ↔ ArrayBuffer codecs (RFC 4648 §5) for WebAuthn payloads:
 * server options arrive base64url-encoded and navigator.credentials
 * consumes/produces raw ArrayBuffers. Single shared copy — previously
 * duplicated privately in Login.tsx and MFASetup.tsx.
 */

export function base64urlToBuffer(base64url: string): ArrayBuffer {
  const base64 = base64url.replaceAll('-', '+').replaceAll('_', '/');
  const pad = base64.length % 4 === 0 ? '' : '='.repeat(4 - (base64.length % 4));
  const binary = atob(base64 + pad);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) {
    bytes[i] = binary.codePointAt(i) ?? 0;
  }
  return bytes.buffer;
}

export function bufferToBase64url(buffer: ArrayBuffer): string {
  const bytes = new Uint8Array(buffer);
  let binary = '';
  for (const byte of bytes) {
    binary += String.fromCodePoint(byte);
  }
  const base64 = btoa(binary).replaceAll('+', '-').replaceAll('/', '_');
  // Linear trailing-'=' trim (a /=+$/ regex risks super-linear backtracking).
  let end = base64.length;
  while (end > 0 && base64.codePointAt(end - 1) === 0x3d) end--;
  return base64.slice(0, end);
}
