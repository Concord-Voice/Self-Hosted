// Client for the host agent's setup-mode API (#187). The token is the
// one-time value the agent printed on the server; it is sent only as a
// bearer header and kept only in memory.

export interface PreflightCheck {
  name: string;
  ok: boolean;
}

export interface Preflight {
  ok: boolean;
  checks: PreflightCheck[];
}

export interface InstallResult {
  ok: boolean;
  output: string;
}

export type Answers = Record<string, string>;

export class SetupError extends Error {
  constructor(
    readonly status: number,
    readonly reason: string,
  ) {
    super(`setup request failed: ${status} ${reason}`);
    this.name = "SetupError";
  }
}

async function request(
  path: string,
  token: string,
  init: RequestInit = {},
): Promise<unknown> {
  const response = await fetch(path, {
    ...init,
    cache: "no-store",
    credentials: "omit",
    headers: { ...init.headers, Authorization: `Bearer ${token}` },
  });
  const body: unknown = await response.json().catch(() => null);
  if (!response.ok) {
    const reason =
      body && typeof body === "object" && "error" in body
        ? String(body.error)
        : "unknown";
    throw new SetupError(response.status, reason);
  }
  return body;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

export function parsePreflight(value: unknown): Preflight {
  if (
    !isRecord(value) ||
    typeof value.ok !== "boolean" ||
    !Array.isArray(value.checks) ||
    !value.checks.every(
      (c) =>
        isRecord(c) && typeof c.name === "string" && typeof c.ok === "boolean",
    )
  ) {
    throw new SetupError(0, "unreadable_response");
  }
  return {
    ok: value.ok,
    checks: (value.checks as PreflightCheck[]).map(({ name, ok }) => ({
      name,
      ok,
    })),
  };
}

export function parseInstall(value: unknown): InstallResult {
  if (
    !isRecord(value) ||
    typeof value.ok !== "boolean" ||
    typeof value.output !== "string"
  ) {
    throw new SetupError(0, "unreadable_response");
  }
  return { ok: value.ok, output: value.output };
}

export const setupApi = {
  async preflight(token: string, signal?: AbortSignal): Promise<Preflight> {
    return parsePreflight(await request("/api/preflight", token, { signal }));
  },
  async install(token: string, answers: Answers): Promise<InstallResult> {
    return parseInstall(
      await request("/api/install", token, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(answers),
      }),
    );
  },
};
