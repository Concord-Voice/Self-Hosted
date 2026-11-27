import { useEffect, useRef, useState } from "react";

import { ApiContractError, ApiError, type SignInNotice, api } from "./api";
import {
  KEY_NOT_USED,
  decodeRequestOptions,
  encodeAssertion,
  keyNotUsed,
} from "./webauthn";

interface AuthScreenProps {
  // notice is what the sign-in response says about admins added since this
  // admin last acknowledged a sign-in notice.
  onAuthenticated: (notice: SignInNotice) => void;
}

interface Ceremony {
  handle: string;
  options: PublicKeyCredentialRequestOptions;
}

// Shown when the server answered but sent a shape this console rejects.
// Distinct from both the credential and the security-key messages on purpose:
// blaming either would send the operator hunting the wrong fault (#3005).
const UNREADABLE_RESPONSE =
  "The server's response was not recognized. This console may be out of date.";

export function AuthScreen({ onAuthenticated }: Readonly<AuthScreenProps>) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [status, setStatus] = useState("");
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);
  const errorRef = useRef<HTMLParagraphElement>(null);
  const [ceremony, setCeremony] = useState<Ceremony | null>(null);

  useEffect(() => {
    if (error) errorRef.current?.focus();
  }, [error]);

  const markBusy = (value: boolean) => {
    busyRef.current = value;
    setBusy(value);
  };

  const runWebAuthn = async (next: Ceremony) => {
    markBusy(true);
    setStatus("Touch your security key");
    setError("");
    try {
      const credential = await navigator.credentials.get({
        publicKey: next.options,
      });
      if (!credential) {
        setError("Security key did not return a credential.");
        return;
      }
      const notice = await api.webauthnLogin(
        next.handle,
        encodeAssertion(credential as PublicKeyCredential),
      );
      setPassword("");
      setStatus("");
      setCeremony(null);
      onAuthenticated(notice);
    } catch (cause) {
      if (keyNotUsed(cause)) {
        setError(KEY_NOT_USED);
        return;
      }
      setCeremony(null);
      if (cause instanceof ApiContractError) {
        setError(UNREADABLE_RESPONSE);
      } else {
        setError("Sign-in failed. Check your credentials and try again.");
      }
    } finally {
      markBusy(false);
    }
  };

  const submitPassword = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (busyRef.current) return;
    markBusy(true);
    setError("");
    setStatus("Checking credentials");
    try {
      const response = await api.passwordLogin(username, password);
      const next = {
        handle: response.handle,
        options: decodeRequestOptions(response.publicKey),
      };
      setCeremony(next);
      await runWebAuthn(next);
    } catch (cause) {
      if (cause instanceof ApiContractError) {
        setError(UNREADABLE_RESPONSE);
      } else if (cause instanceof ApiError) {
        setError("Sign-in failed. Check your credentials and try again.");
      } else {
        setError("Security-key challenge could not be used. Start again.");
      }
      setStatus("");
      markBusy(false);
    }
  };

  const backToPassword = () => {
    setCeremony(null);
    setError("");
    setStatus("");
  };

  return (
    <section aria-labelledby="auth-title" className="auth-panel">
      <h1 id="auth-title">Admin sign in</h1>
      {error ? (
        <p ref={errorRef} role="alert" className="form-error" tabIndex={-1}>
          {error}
        </p>
      ) : null}
      {status ? <output className="form-status">{status}</output> : null}
      <form onSubmit={submitPassword}>
        <label htmlFor="admin-username">Username</label>
        <input
          id="admin-username"
          autoComplete="username"
          disabled={busy}
          value={username}
          onChange={(event) => setUsername(event.currentTarget.value)}
        />
        <label htmlFor="admin-password">Password</label>
        <input
          id="admin-password"
          autoComplete="current-password"
          disabled={busy}
          type="password"
          value={password}
          onChange={(event) => setPassword(event.currentTarget.value)}
        />
        <button disabled={busy || !username || !password} type="submit">
          {busy ? "Verifying" : "Continue"}
        </button>
      </form>
      {ceremony && !busy ? (
        <div className="button-row">
          <button type="button" onClick={() => void runWebAuthn(ceremony)}>
            Retry security key
          </button>
          <button type="button" onClick={backToPassword}>
            Back to password
          </button>
        </div>
      ) : null}
    </section>
  );
}
