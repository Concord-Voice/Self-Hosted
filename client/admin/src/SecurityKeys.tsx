import { useCallback, useEffect, useRef, useState } from "react";

import { ApiContractError, ApiError, api, type SecurityKeyList } from "./api";
import { formatTimestamp, type TimeMode } from "./time";
import {
  KEY_NOT_USED,
  decodeCreationOptions,
  decodeRequestOptions,
  encodeAssertion,
  encodeAttestation,
  keyNotUsed,
} from "./webauthn";

interface SecurityKeysProps {
  onForbidden: () => void;
  onUnauthorized: () => void;
  timeMode: TimeMode;
}

// Shown after a key is added. Every key signs in at once, but the host agent
// trusts only the keys root last imported.
export const REIMPORT_NOTICE =
  "To use this key for server actions, a host operator must run the export and import again.";

const UNREADABLE_RESPONSE =
  "The server's response was not recognized. This console may be out of date.";

// What each refusal code means to the operator. Only the table's own keys
// count, so a code such as "constructor" falls back to the generic line.
const REFUSALS: Record<string, string> = {
  credential_limit_reached:
    "You already have the maximum number of security keys.",
  credential_already_registered: "This security key is already registered.",
  stepup_required: "Confirm with one of your current security keys first.",
  stepup_failed:
    "The check with your current security key failed. Start again.",
  stepup_unavailable: "You have no security key to confirm with.",
  registration_failed:
    "The new security key was not accepted. Use a different, allowed key and start again.",
};

function invalidState(cause: unknown): boolean {
  return cause instanceof DOMException && cause.name === "InvalidStateError";
}

export function SecurityKeys({
  onForbidden,
  onUnauthorized,
  timeMode,
}: Readonly<SecurityKeysProps>) {
  const [list, setList] = useState<SecurityKeyList | null>(null);
  const [loadError, setLoadError] = useState("");
  const [error, setError] = useState("");
  const [status, setStatus] = useState("");
  const [added, setAdded] = useState(false);
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);
  const errorRef = useRef<HTMLParagraphElement>(null);

  useEffect(() => {
    if (error) errorRef.current?.focus();
  }, [error]);

  // Returns the copy to show, or null when the session itself ended.
  const describe = useCallback(
    (cause: unknown): string | null => {
      if (cause instanceof ApiError && cause.status === 401) {
        onUnauthorized();
        return null;
      }
      if (
        cause instanceof ApiError &&
        cause.code !== null &&
        Object.hasOwn(REFUSALS, cause.code)
      ) {
        return REFUSALS[cause.code];
      }
      if (cause instanceof ApiError && cause.status === 403) {
        onForbidden();
        return null;
      }
      if (cause instanceof ApiContractError) return UNREADABLE_RESPONSE;
      if (cause instanceof ApiError && cause.status === 429) {
        return "Too many attempts. Wait a minute and try again.";
      }
      return "Your security keys could not be loaded or changed. Try again.";
    },
    [onForbidden, onUnauthorized],
  );

  const load = useCallback(
    async (signal?: AbortSignal) => {
      try {
        const next = await api.listSecurityKeys({ signal });
        setList(next);
        setLoadError("");
      } catch (cause) {
        if (signal?.aborted) return;
        setLoadError(describe(cause) ?? "");
      }
    },
    [describe],
  );

  useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [load]);

  const full = list !== null && list.keys.length >= list.max;

  const addKey = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (busyRef.current) return;
    busyRef.current = true;
    setBusy(true);
    setError("");
    setAdded(false);
    try {
      setStatus("Touch one of your current security keys to confirm");
      const challenge = await api.securityKeyStepUp();
      const assertion = await navigator.credentials.get({
        publicKey: decodeRequestOptions(challenge.publicKey),
      });
      if (!assertion)
        throw new DOMException("no credential", "NotAllowedError");
      const creation = await api.securityKeyRegisterBegin(
        challenge.handle,
        encodeAssertion(assertion as PublicKeyCredential),
      );
      setStatus("Now touch the new security key");
      const credential = await navigator.credentials.create({
        publicKey: decodeCreationOptions(creation.publicKey),
      });
      if (!credential)
        throw new DOMException("no credential", "NotAllowedError");
      await api.securityKeyRegisterFinish(
        creation.handle,
        encodeAttestation(credential as PublicKeyCredential),
        name,
      );
      setName("");
      setStatus("");
      setAdded(true);
      await load();
    } catch (cause) {
      setStatus("");
      if (keyNotUsed(cause)) {
        setError(KEY_NOT_USED);
      } else if (invalidState(cause)) {
        // The browser refused a key that already holds one of this admin's
        // credentials (excludeCredentials).
        setError(
          "That security key is already registered. Use a different key as your backup.",
        );
      } else {
        const copy = describe(cause);
        if (copy) setError(copy);
      }
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  };

  return (
    <div className="workspace-content">
      <section
        aria-labelledby="security-keys-title"
        className="workspace-section"
      >
        <div className="section-heading">
          <div>
            <h2 id="security-keys-title">Security keys</h2>
            <p>
              Each key signs you in. Keep a backup key, a different physical
              key, in case the first is lost.
            </p>
          </div>
        </div>
        {loadError ? (
          <p role="alert" className="form-error">
            {loadError}
          </p>
        ) : null}
        {list === null && !loadError ? (
          <output className="form-status">Loading security keys</output>
        ) : null}
        {list ? (
          <table className="security-keys-table">
            <caption>
              Your security keys ({list.keys.length} of {list.max})
            </caption>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Added</th>
              </tr>
            </thead>
            <tbody>
              {list.keys.map((key) => (
                <tr key={key.id}>
                  <td>{key.name || "Unnamed key"}</td>
                  <td>{formatTimestamp(key.createdAt, timeMode)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : null}
        {error ? (
          <p ref={errorRef} role="alert" className="form-error" tabIndex={-1}>
            {error}
          </p>
        ) : null}
        {status ? <output className="form-status">{status}</output> : null}
        {added ? (
          <p role="status" className="form-status">
            Security key added. {REIMPORT_NOTICE}
          </p>
        ) : null}
        {full ? (
          <p role="status">
            You have the maximum of {list.max} security keys. No more can be
            added.
          </p>
        ) : null}
        {list && !full ? (
          <form className="series-controls" onSubmit={addKey}>
            <label>
              <span>New key name</span>
              <input
                autoComplete="off"
                disabled={busy}
                maxLength={64}
                value={name}
                onChange={(event) => setName(event.currentTarget.value)}
              />
            </label>
            <button
              className="server-action"
              disabled={busy || !name.trim()}
              type="submit"
            >
              {busy ? "Adding key" : "Add a backup key"}
            </button>
          </form>
        ) : null}
      </section>
    </div>
  );
}
