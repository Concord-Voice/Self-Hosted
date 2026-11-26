import {
  ContractError,
  parseCounters,
  parseCurrent,
  parseHealth,
  parseSeries,
} from "./contracts";
import type {
  AdminCountersResponse,
  AdminCurrentResponse,
  AdminHealthResponse,
  AdminSeriesResponse,
  MetricKey,
  SeriesWindow,
} from "./contracts";

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    public readonly retryAfter: number | null,
    // The server's stable error code, when its error body names one.
    public readonly code: string | null = null,
  ) {
    super("Admin Portal request failed");
    this.name = "ApiError";
  }
}

export class ApiContractError extends Error {
  constructor() {
    super("Admin Portal response did not match the request");
    this.name = "ApiContractError";
  }
}

interface CredentialOptionsResponse {
  handle: string;
  publicKey: unknown;
}

export type PasswordLoginResponse = CredentialOptionsResponse;
export type EnrollBeginResponse = CredentialOptionsResponse;

interface RequestOptions {
  signal?: AbortSignal;
}

interface RequestInitOptions extends RequestOptions {
  body?: unknown;
  method?: "GET" | "POST";
}

export function retryAfter(value: string | null): number | null {
  if (value === null || !/^\d+$/.test(value)) return null;
  const seconds = Number(value);
  return Number.isSafeInteger(seconds) && seconds <= 3600 ? seconds : null;
}

async function errorCode(response: Response): Promise<string | null> {
  try {
    const value: unknown = await response.json();
    if (typeof value === "object" && value !== null && "error" in value) {
      const { error } = value as { error: unknown };
      return typeof error === "string" ? error : null;
    }
  } catch {
    // An error without a JSON body carries no code.
  }
  return null;
}

async function request<T>(
  path: string,
  parser: (value: unknown) => T,
  init: RequestInitOptions = {},
): Promise<T> {
  const headers = new Headers();
  let body: string | undefined;
  if (init.body !== undefined) {
    headers.set("Content-Type", "application/json");
    body = JSON.stringify(init.body);
  }

  let response: Response;
  try {
    response = await fetch(path, {
      method: init.method ?? (init.body === undefined ? "GET" : "POST"),
      credentials: "same-origin",
      headers,
      body,
      signal: init.signal,
    });
  } catch {
    throw new ApiError(0, null);
  }

  if (!response.ok) {
    throw new ApiError(
      response.status,
      retryAfter(response.headers.get("Retry-After")),
      await errorCode(response),
    );
  }

  if (response.status === 204) return parser(null);

  let value: unknown;
  try {
    value = await response.json();
  } catch {
    throw new ApiError(0, null);
  }
  try {
    return parser(value);
  } catch (cause) {
    // A shape we REJECTED is not a transport failure. ContractError is thrown
    // by the parsers in contracts.ts; without this it fell through to
    // ApiError(0), which usePolling maps to the stale "live telemetry is
    // unavailable" banner — so a client/server schema mismatch was reported as
    // a dead backend while the server answered 200 (#3004). getSeries already
    // threw ApiContractError for a mismatched key or window; this makes the
    // parse path agree with it.
    if (cause instanceof ApiContractError || cause instanceof ContractError) {
      throw new ApiContractError();
    }
    throw new ApiError(0, null);
  }
}

// These four validators run as the `parser` argument to request(), so throwing
// ContractError lets request() classify them exactly as it classifies a
// rejected metrics shape. Throwing ApiError(0) here instead would have left
// api.ts classifying the SAME failure differently per endpoint — auth as a
// transport failure, metrics as a contract failure.
function record(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new ContractError();
  }
  return value as Record<string, unknown>;
}

function exactKeys(
  value: Record<string, unknown>,
  expected: readonly string[],
): void {
  const actual = Object.keys(value);
  const expectedSet = new Set(expected);
  if (
    actual.length !== expected.length ||
    actual.some((key) => !expectedSet.has(key))
  ) {
    throw new ContractError();
  }
}

function nonEmptyString(value: unknown): string {
  if (typeof value !== "string" || value.length === 0) {
    throw new ContractError();
  }
  return value;
}

function credentialOptionsResponse(value: unknown): CredentialOptionsResponse {
  const data = record(value);
  exactKeys(data, ["handle", "publicKey"]);
  record(data.publicKey);
  return {
    handle: nonEmptyString(data.handle),
    publicKey: data.publicKey,
  };
}

// The sign-in response caps admins_added at this value; the portal shows the
// cap as "N or more".
export const MAX_ADMINS_ADDED_NOTICE = 100;

// What a sign-in tells the admin: how many admins were added since the last
// notice they acknowledged, and the opaque cursor that acknowledges it (null
// when the count is 0, which has nothing to acknowledge).
export interface SignInNotice {
  adminsAdded: number;
  cursor: string | null;
}

const NOTICE_CURSOR = /^[1-9]\d{0,17}$/;

// The sign-in success carries the count beside its status, and the cursor
// exactly when the count is above 0. The server leaves both out when it could
// not read the count, rather than block the sign-in, and when the request does
// not set signin_notice, which this portal always does; the portal then shows
// no notice. When present they must be valid.
function signInResponse(value: unknown): SignInNotice {
  const data = record(value);
  if (data.status !== "authenticated") throw new ContractError();
  if (!("admins_added" in data)) {
    exactKeys(data, ["status"]);
    return { adminsAdded: 0, cursor: null };
  }
  const count = data.admins_added;
  if (
    typeof count !== "number" ||
    !Number.isSafeInteger(count) ||
    count < 0 ||
    count > MAX_ADMINS_ADDED_NOTICE
  ) {
    throw new ContractError();
  }
  if (count === 0) {
    exactKeys(data, ["status", "admins_added"]);
    return { adminsAdded: 0, cursor: null };
  }
  exactKeys(data, ["status", "admins_added", "notice_cursor"]);
  const cursor = data.notice_cursor;
  if (typeof cursor !== "string" || !NOTICE_CURSOR.test(cursor)) {
    throw new ContractError();
  }
  return { adminsAdded: count, cursor };
}

function noContent(value: unknown): void {
  if (value !== null) throw new ContractError();
}

function statusResponse(expected: string): (value: unknown) => void {
  return (value) => {
    const data = record(value);
    exactKeys(data, ["status"]);
    if (data.status !== expected) throw new ContractError();
  };
}

export const api = {
  getHealth(options?: RequestOptions): Promise<AdminHealthResponse> {
    return request("/admin/api/v1/health", parseHealth, options);
  },
  getCurrent(options?: RequestOptions): Promise<AdminCurrentResponse> {
    return request("/admin/api/v1/metrics/current", parseCurrent, options);
  },
  getCounters(options?: RequestOptions): Promise<AdminCountersResponse> {
    return request("/admin/api/v1/counters", parseCounters, options);
  },
  getSeries(
    metricKey: MetricKey,
    window: SeriesWindow,
    options?: RequestOptions,
  ): Promise<AdminSeriesResponse> {
    const query = new URLSearchParams({ key: metricKey, window });
    return request(
      `/admin/api/v1/metrics/series?${query.toString()}`,
      (value) => {
        const response = parseSeries(value);
        if (
          response.metric.metric_key !== metricKey ||
          response.window !== window
        ) {
          throw new ApiContractError();
        }
        return response;
      },
      options,
    );
  },
  passwordLogin(
    username: string,
    password: string,
    options?: RequestOptions,
  ): Promise<PasswordLoginResponse> {
    return request("/admin/api/v1/auth/password", credentialOptionsResponse, {
      ...options,
      body: { username, password },
    });
  },
  webauthnLogin(
    handle: string,
    assertion: unknown,
    options?: RequestOptions,
  ): Promise<SignInNotice> {
    return request("/admin/api/v1/auth/webauthn", signInResponse, {
      ...options,
      // The server returns the notice fields only on request, because a tab
      // loaded before they existed rejects any field beside status.
      body: { handle, assertion, signin_notice: true },
    });
  },
  // Tells the server this session displayed its sign-in notice, so the next
  // sign-in counts only the admins added after it.
  ackSignInNotice(cursor: string, options?: RequestOptions): Promise<void> {
    return request("/admin/api/v1/signin-notice/ack", noContent, {
      ...options,
      body: { cursor },
    });
  },
  enrollBegin(
    username: string,
    password: string,
    token: string,
    options?: RequestOptions,
  ): Promise<EnrollBeginResponse> {
    return request("/admin/api/v1/enroll/begin", credentialOptionsResponse, {
      ...options,
      body: { username, password, token },
    });
  },
  enrollFinish(
    handle: string,
    attestation: unknown,
    credentialName: string,
    options?: RequestOptions,
  ): Promise<void> {
    return request("/admin/api/v1/enroll/finish", statusResponse("enrolled"), {
      ...options,
      body: { handle, attestation, credential_name: credentialName },
    });
  },
  logout(options?: RequestOptions): Promise<void> {
    return request("/admin/api/v1/auth/logout", statusResponse("logged out"), {
      ...options,
      method: "POST",
    });
  },
};
