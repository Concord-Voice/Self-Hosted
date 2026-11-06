import { useEffect, useRef, useState } from "react";

import {
  type Answers,
  type InstallResult,
  type Preflight,
  SetupError,
  setupApi,
} from "./setupApi";

type TlsMode = "letsencrypt" | "local" | "import";
type StorageMode = "bundled" | "byo-s3";

export interface SetupForm {
  domain: string;
  publicIp: string;
  tlsMode: TlsMode;
  smtpHost: string;
  smtpPort: string;
  smtpUser: string;
  smtpPass: string;
  smtpFrom: string;
  storageMode: StorageMode;
  storageEndpoint: string;
  storageRegion: string;
  storageBucket: string;
  storageAccessKey: string;
  storageSecretKey: string;
  storageUseSsl: boolean;
}

const EMPTY_FORM: SetupForm = {
  domain: "",
  publicIp: "",
  tlsMode: "letsencrypt",
  smtpHost: "",
  smtpPort: "",
  smtpUser: "",
  smtpPass: "",
  smtpFrom: "",
  storageMode: "bundled",
  storageEndpoint: "",
  storageRegion: "",
  storageBucket: "",
  storageAccessKey: "",
  storageSecretKey: "",
  storageUseSsl: true,
};

// Plain-word names for the installer's preflight checks.
const CHECK_LABELS: Record<string, string> = {
  os: "Supported operating system",
  docker: "Docker and Docker Compose are installed",
  resources: "Enough memory and disk space",
  logrotate: "Log rotation (logrotate) is installed",
  ports: "The ports Concord Voice needs are free",
};

const TLS_HINTS: Record<TlsMode, string> = {
  letsencrypt:
    "Free certificate from Let's Encrypt. Needs DNS pointing here and port 80 open.",
  local: "Self-signed. Only for a trusted home or office network.",
  import: "Use a certificate you already have. You add the files after setup.",
};

// buildAnswers maps the form onto the installer's answers fields. Empty
// optional fields are left out so the installer applies its own defaults.
export function buildAnswers(form: SetupForm): Answers {
  const answers: Answers = {
    CFG_DOMAIN: form.domain.trim(),
    CFG_PUBLIC_IP: form.publicIp.trim(),
    CFG_TLS_MODE: form.tlsMode,
    CFG_SMTP_HOST: form.smtpHost.trim(),
    CFG_STORAGE_MODE: form.storageMode,
  };
  const optional: [string, string][] = [
    ["CFG_SMTP_PORT", form.smtpPort.trim()],
    ["CFG_SMTP_USER", form.smtpUser],
    ["CFG_SMTP_PASS", form.smtpPass],
    ["CFG_SMTP_FROM", form.smtpFrom.trim()],
  ];
  if (form.storageMode === "byo-s3") {
    optional.push(
      ["CFG_STORAGE_ENDPOINT", form.storageEndpoint.trim()],
      ["CFG_STORAGE_REGION", form.storageRegion.trim()],
      ["CFG_STORAGE_BUCKET", form.storageBucket.trim()],
      ["CFG_STORAGE_ACCESS_KEY", form.storageAccessKey],
      ["CFG_STORAGE_SECRET_KEY", form.storageSecretKey],
      ["CFG_STORAGE_USE_SSL", form.storageUseSsl ? "true" : "false"],
    );
  }
  for (const [key, value] of optional) {
    if (value !== "") answers[key] = value;
  }
  return answers;
}

const TOKEN_COMMAND = "sudo cat /run/concord-agent/setup-token";

export function errorMessage(cause: unknown): string {
  if (cause instanceof SetupError) {
    switch (cause.status) {
      case 400:
        return "An answer has a line break or a character the installer cannot take. Check the answers and try again.";
      case 401:
        return `That token is not right. On the server, run ${TOKEN_COMMAND} and copy it again.`;
      case 429:
        return "That token is not right, and many wrong tokens were tried in the last 10 minutes. The right token still works.";
      case 410:
        return "Setup is already complete on this server. The installer's output, with the next steps, is in the server's journal: journalctl -u concord-agent.";
      case 409:
        return cause.reason === "preflight_running"
          ? "The checks are already running. Wait for them to finish."
          : "An install is already running. Wait for it to finish.";
    }
  }
  return "The server could not complete that step. Check the server is still running and try again.";
}

function tokenFromHash(): string {
  const match = /(?:^#|&)token=([0-9a-f]{64})(?:&|$)/.exec(
    globalThis.location.hash,
  );
  if (match) {
    // Keep the token out of history and anything copied from the address bar.
    globalThis.history.replaceState(null, "", globalThis.location.pathname);
    return match[1];
  }
  return "";
}

type Step = "token" | "preflight" | "answers" | "result";

export function SetupWizard() {
  const [initialToken] = useState(tokenFromHash);
  const [token, setToken] = useState(initialToken);
  const [step, setStep] = useState<Step>(initialToken ? "preflight" : "token");
  const [preflight, setPreflight] = useState<Preflight | null>(null);
  const [form, setForm] = useState<SetupForm>(EMPTY_FORM);
  const [result, setResult] = useState<InstallResult | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const errorRef = useRef<HTMLParagraphElement>(null);
  const abortRef = useRef<AbortController | null>(null);
  const panelRef = useRef<HTMLElement>(null);
  const firstStepRef = useRef(true);

  useEffect(() => {
    if (error) errorRef.current?.focus();
  }, [error]);

  // The button that moved the wizard on is gone, so move focus to the new
  // step's heading and let a screen reader announce it. An error keeps focus.
  useEffect(() => {
    if (firstStepRef.current) {
      firstStepRef.current = false;
      return;
    }
    if (!errorRef.current) panelRef.current?.querySelector("h2")?.focus();
  }, [step]);

  useEffect(() => () => abortRef.current?.abort(), []);

  const runPreflight = async (value: string) => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setBusy(true);
    setError("");
    try {
      const next = await setupApi.preflight(value, controller.signal);
      setPreflight(next);
      setStep("preflight");
    } catch (cause) {
      if (controller.signal.aborted) return;
      setPreflight(null);
      setError(errorMessage(cause));
      if (cause instanceof SetupError && cause.status === 401) setStep("token");
    } finally {
      if (!controller.signal.aborted) setBusy(false);
    }
  };

  useEffect(() => {
    if (initialToken) void runPreflight(initialToken);
    // initialToken never changes, so this runs once, for a token from the link.
  }, [initialToken]);

  const install = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (busy) return;
    setBusy(true);
    setError("");
    try {
      const next = await setupApi.install(token, buildAnswers(form));
      setResult(next);
      setStep("result");
      if (next.ok) setForm(EMPTY_FORM);
    } catch (cause) {
      setError(errorMessage(cause));
      // A new token (after a restart) is entered on the token step; the
      // answers stay in the form.
      if (cause instanceof SetupError && cause.status === 401) setStep("token");
    } finally {
      setBusy(false);
    }
  };

  return (
    <main className="app-main setup-main">
      <section
        ref={panelRef}
        aria-labelledby="setup-title"
        className="auth-panel setup-panel"
      >
        <h1 id="setup-title">Set up Concord Voice</h1>
        {error ? (
          <p ref={errorRef} role="alert" className="form-error" tabIndex={-1}>
            {error}
          </p>
        ) : null}

        {step === "token" ? (
          <form
            onSubmit={(event) => {
              event.preventDefault();
              void runPreflight(token.trim());
            }}
          >
            <h2 tabIndex={-1}>Setup token</h2>
            <p className="form-status">
              On the server, run <code>{TOKEN_COMMAND}</code> and paste the
              token from the address it shows. Check the certificate fingerprint
              your browser shows matches the one in the server's journal.
            </p>
            <label htmlFor="setup-token">Setup token</label>
            <input
              id="setup-token"
              autoComplete="off"
              disabled={busy}
              spellCheck={false}
              type="password"
              value={token}
              onChange={(event) => setToken(event.currentTarget.value)}
            />
            <button disabled={busy || token.trim() === ""} type="submit">
              {busy ? "Checking" : "Continue"}
            </button>
          </form>
        ) : null}

        {step === "preflight" ? (
          <ChecksStep
            busy={busy}
            preflight={preflight}
            onContinue={() => setStep("answers")}
            onRecheck={() => void runPreflight(token)}
          />
        ) : null}

        {step === "answers" ? (
          <AnswersForm
            busy={busy}
            form={form}
            setForm={setForm}
            onSubmit={(event) => void install(event)}
          />
        ) : null}

        {step === "result" && result ? (
          <ResultStep result={result} onBack={() => setStep("answers")} />
        ) : null}
      </section>
    </main>
  );
}

interface ChecksStepProps {
  busy: boolean;
  preflight: Preflight | null;
  onRecheck: () => void;
  onContinue: () => void;
}

function ChecksStep({
  busy,
  preflight,
  onRecheck,
  onContinue,
}: Readonly<ChecksStepProps>) {
  return (
    <>
      <h2 tabIndex={-1}>Server checks</h2>
      {preflight ? (
        <ul className="setup-checks">
          {preflight.checks.map((check) => (
            <li key={check.name}>
              <span
                className={`status ${check.ok ? "status-healthy" : "status-critical"}`}
              >
                {check.ok ? "Passed" : "Failed"}
              </span>{" "}
              {CHECK_LABELS[check.name] ?? check.name}
            </li>
          ))}
        </ul>
      ) : null}
      {!preflight && busy ? (
        <output className="form-status">Running checks</output>
      ) : null}
      {preflight && !preflight.ok ? (
        <p className="form-status">
          Fix the failed checks on the server, then check again. The server's
          journal has the details.
        </p>
      ) : null}
      <div className="button-row">
        <button disabled={busy} type="button" onClick={onRecheck}>
          {busy ? "Checking" : "Check again"}
        </button>
        <button
          disabled={busy || !preflight?.ok}
          type="button"
          onClick={onContinue}
        >
          Continue
        </button>
      </div>
    </>
  );
}

interface AnswersFormProps {
  busy: boolean;
  form: SetupForm;
  setForm: React.Dispatch<React.SetStateAction<SetupForm>>;
  onSubmit: (event: React.FormEvent<HTMLFormElement>) => void;
}

function AnswersForm({
  busy,
  form,
  setForm,
  onSubmit,
}: Readonly<AnswersFormProps>) {
  const field = (key: keyof SetupForm) => ({
    id: `setup-${key}`,
    disabled: busy,
    value: form[key] as string,
    onChange: (event: React.ChangeEvent<HTMLInputElement>) => {
      const value = event.currentTarget.value;
      setForm((prev) => ({ ...prev, [key]: value }));
    },
  });

  return (
    <form onSubmit={onSubmit}>
      <h2 tabIndex={-1}>Server address</h2>
      <label htmlFor="setup-domain">Domain (for example example.com)</label>
      <input {...field("domain")} autoComplete="off" required />
      <label htmlFor="setup-publicIp">Public IPv4 address</label>
      <input
        {...field("publicIp")}
        autoComplete="off"
        inputMode="decimal"
        required
      />

      <fieldset disabled={busy}>
        <legend>HTTPS certificate</legend>
        {(Object.keys(TLS_HINTS) as TlsMode[]).map((mode) => (
          <label key={mode} className="setup-choice">
            <input
              checked={form.tlsMode === mode}
              name="tls-mode"
              type="radio"
              value={mode}
              onChange={() => setForm((prev) => ({ ...prev, tlsMode: mode }))}
            />
            <span>{TLS_HINTS[mode]}</span>
          </label>
        ))}
      </fieldset>

      <h2>Email (SMTP)</h2>
      <label htmlFor="setup-smtpHost">SMTP server</label>
      <input {...field("smtpHost")} autoComplete="off" required />
      <label htmlFor="setup-smtpPort">Port (optional)</label>
      <input {...field("smtpPort")} autoComplete="off" inputMode="numeric" />
      <label htmlFor="setup-smtpUser">Username (optional)</label>
      <input {...field("smtpUser")} autoComplete="off" />
      <label htmlFor="setup-smtpPass">Password (optional)</label>
      <input
        {...field("smtpPass")}
        autoComplete="new-password"
        type="password"
      />
      <label htmlFor="setup-smtpFrom">Sender address (optional)</label>
      <input {...field("smtpFrom")} autoComplete="off" type="email" />

      <fieldset disabled={busy}>
        <legend>File storage</legend>
        <label className="setup-choice">
          <input
            checked={form.storageMode === "bundled"}
            name="storage-mode"
            type="radio"
            onChange={() =>
              setForm((prev) => ({ ...prev, storageMode: "bundled" }))
            }
          />
          <span>Store files on this server</span>
        </label>
        <label className="setup-choice">
          <input
            checked={form.storageMode === "byo-s3"}
            name="storage-mode"
            type="radio"
            onChange={() =>
              setForm((prev) => ({ ...prev, storageMode: "byo-s3" }))
            }
          />
          <span>Use my own S3-compatible storage</span>
        </label>
      </fieldset>
      {form.storageMode === "byo-s3" ? (
        <>
          <label htmlFor="setup-storageEndpoint">Endpoint</label>
          <input {...field("storageEndpoint")} autoComplete="off" required />
          <label htmlFor="setup-storageRegion">Region (optional)</label>
          <input {...field("storageRegion")} autoComplete="off" />
          <label htmlFor="setup-storageBucket">Bucket (optional)</label>
          <input {...field("storageBucket")} autoComplete="off" />
          <label htmlFor="setup-storageAccessKey">Access key</label>
          <input {...field("storageAccessKey")} autoComplete="off" required />
          <label htmlFor="setup-storageSecretKey">Secret key</label>
          <input
            {...field("storageSecretKey")}
            autoComplete="new-password"
            required
            type="password"
          />
          <label className="setup-choice">
            <input
              checked={form.storageUseSsl}
              disabled={busy}
              type="checkbox"
              onChange={(event) => {
                const checked = event.currentTarget.checked;
                setForm((prev) => ({ ...prev, storageUseSsl: checked }));
              }}
            />
            <span>Connect with HTTPS</span>
          </label>
        </>
      ) : null}
      <button disabled={busy} type="submit">
        {busy ? "Installing" : "Install"}
      </button>
    </form>
  );
}

interface ResultStepProps {
  result: InstallResult;
  onBack: () => void;
}

function resultText(result: InstallResult): [string, string] {
  if (!result.ok) {
    return [
      "The installer stopped",
      "Read the installer's message below, fix the answers, and try again.",
    ];
  }
  if (!result.clean) {
    return [
      "Concord Voice is configured, but the installer reported an error",
      "This setup page has closed. The installer saved the settings, then stopped at a later step. Read its message below and finish on the server.",
    ];
  }
  return [
    "Concord Voice is configured",
    "This setup page has closed. Finish on the server with the steps below.",
  ];
}

function ResultStep({ result, onBack }: Readonly<ResultStepProps>) {
  const [heading, text] = resultText(result);
  return (
    <>
      <h2 tabIndex={-1}>{heading}</h2>
      <p className="form-status">{text}</p>
      <textarea
        aria-label="Installer output"
        className="server-logs"
        readOnly
        rows={16}
        value={result.output === "" ? "No output." : result.output}
      />
      {result.ok ? null : (
        <div className="button-row">
          <button type="button" onClick={onBack}>
            Back to answers
          </button>
        </div>
      )}
    </>
  );
}
