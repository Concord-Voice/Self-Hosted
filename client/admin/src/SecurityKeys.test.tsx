import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { REIMPORT_NOTICE, SecurityKeys } from "./SecurityKeys";
import { server } from "./test/server";

const base = "http://localhost/admin/api/v1/credentials";
const primary = {
  id: "11111111-1111-4111-8111-111111111111",
  name: "YubiKey desk",
  created_at: "2026-11-20T10:00:00Z",
};
const backup = {
  id: "22222222-2222-4222-8222-222222222222",
  name: "YubiKey safe",
  created_at: "2026-11-26T10:00:00.123456Z",
};

const stepUpOptions = {
  handle: "s".repeat(64),
  publicKey: {
    publicKey: {
      challenge: "AQID",
      rpId: "localhost",
      allowCredentials: [{ type: "public-key", id: "BAUG" }],
      userVerification: "required",
    },
  },
};

const creationOptions = {
  handle: "r".repeat(64),
  publicKey: {
    publicKey: {
      challenge: "AQID",
      rp: { id: "localhost", name: "Concord Voice" },
      user: { id: "BAUG", name: "operator", displayName: "operator" },
      pubKeyCredParams: [{ type: "public-key", alg: -7 }],
      excludeCredentials: [{ type: "public-key", id: "BAUG" }],
      attestation: "direct",
    },
  },
};

function assertionCredential(): PublicKeyCredential {
  return {
    id: "credential",
    rawId: new Uint8Array([1, 2, 3]).buffer,
    type: "public-key",
    response: {
      authenticatorData: new Uint8Array([4]).buffer,
      clientDataJSON: new Uint8Array([5]).buffer,
      signature: new Uint8Array([6]).buffer,
      userHandle: null,
    },
    getClientExtensionResults: () => ({}),
  } as unknown as PublicKeyCredential;
}

function attestationCredential(): PublicKeyCredential {
  return {
    id: "new-credential",
    rawId: new Uint8Array([7, 8, 9]).buffer,
    type: "public-key",
    response: {
      attestationObject: new Uint8Array([4]).buffer,
      clientDataJSON: new Uint8Array([5]).buffer,
    },
    getClientExtensionResults: () => ({}),
  } as unknown as PublicKeyCredential;
}

function renderKeys() {
  const onForbidden = vi.fn();
  const onUnauthorized = vi.fn();
  render(
    <SecurityKeys
      onForbidden={onForbidden}
      onUnauthorized={onUnauthorized}
      timeMode="utc"
    />,
  );
  return { onForbidden, onUnauthorized };
}

describe("SecurityKeys", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it("lists the admin's keys and adds a backup after a step-up", async () => {
    let keys = [primary];
    const bodies: Record<string, unknown> = {};
    server.use(
      http.get(base, () => HttpResponse.json({ credentials: keys, max: 4 })),
      http.post(`${base}/stepup`, () => HttpResponse.json(stepUpOptions)),
      http.post(`${base}/register/begin`, async ({ request }) => {
        bodies.begin = await request.json();
        return HttpResponse.json(creationOptions);
      }),
      http.post(`${base}/register/finish`, async ({ request }) => {
        bodies.finish = await request.json();
        keys = [primary, backup];
        return HttpResponse.json({ status: "registered", credential: backup });
      }),
    );
    const get = vi
      .spyOn(navigator.credentials, "get")
      .mockResolvedValue(assertionCredential());
    const create = vi
      .spyOn(navigator.credentials, "create")
      .mockResolvedValue(attestationCredential());
    renderKeys();

    expect(await screen.findByText("YubiKey desk")).toBeVisible();
    expect(screen.getByText(/1 of 4/)).toBeVisible();
    await userEvent.type(screen.getByLabelText("New key name"), "YubiKey safe");
    await userEvent.click(
      screen.getByRole("button", { name: "Add a backup key" }),
    );

    await waitFor(() =>
      expect(screen.getByRole("status")).toHaveTextContent(REIMPORT_NOTICE),
    );
    expect(REIMPORT_NOTICE).toBe(
      "To use this key for server actions, a host operator must run the export and import again.",
    );
    expect(await screen.findByText("YubiKey safe")).toBeVisible();
    expect(get).toHaveBeenCalledTimes(1);
    expect(bodies.begin).toEqual({
      stepup: { handle: stepUpOptions.handle, assertion: expect.anything() },
    });
    // The browser is handed the server's exclusion list, so it refuses to
    // overwrite a key that already holds one of this admin's credentials.
    const options = create.mock.calls[0]?.[0]?.publicKey;
    expect(options?.excludeCredentials).toHaveLength(1);
    expect(bodies.finish).toMatchObject({
      handle: creationOptions.handle,
      credential_name: "YubiKey safe",
      attestation: { id: "new-credential" },
    });
  });

  it("shows the cap and offers no add when the admin holds the maximum", async () => {
    server.use(
      http.get(base, () =>
        HttpResponse.json({ credentials: [primary, backup], max: 2 }),
      ),
    );
    renderKeys();

    expect(await screen.findByText(/maximum of 2 security keys/)).toBeVisible();
    expect(
      screen.queryByRole("button", { name: "Add a backup key" }),
    ).toBeNull();
  });

  it("explains a 409 cap refusal from the server", async () => {
    server.use(
      http.get(base, () =>
        HttpResponse.json({ credentials: [primary], max: 4 }),
      ),
      http.post(`${base}/stepup`, () => HttpResponse.json(stepUpOptions)),
      http.post(`${base}/register/begin`, () =>
        HttpResponse.json(
          { error: "credential_limit_reached", max: 4 },
          { status: 409 },
        ),
      ),
    );
    vi.spyOn(navigator.credentials, "get").mockResolvedValue(
      assertionCredential(),
    );
    const create = vi.spyOn(navigator.credentials, "create");
    renderKeys();

    await userEvent.type(await screen.findByLabelText("New key name"), "Spare");
    await userEvent.click(
      screen.getByRole("button", { name: "Add a backup key" }),
    );

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "You already have the maximum number of security keys.",
    );
    expect(create).not.toHaveBeenCalled();
  });

  it("explains a failed step-up without ending the session", async () => {
    server.use(
      http.get(base, () =>
        HttpResponse.json({ credentials: [primary], max: 4 }),
      ),
      http.post(`${base}/stepup`, () => HttpResponse.json(stepUpOptions)),
      http.post(`${base}/register/begin`, () =>
        HttpResponse.json({ error: "stepup_failed" }, { status: 403 }),
      ),
    );
    vi.spyOn(navigator.credentials, "get").mockResolvedValue(
      assertionCredential(),
    );
    const { onForbidden } = renderKeys();

    await userEvent.type(await screen.findByLabelText("New key name"), "Spare");
    await userEvent.click(
      screen.getByRole("button", { name: "Add a backup key" }),
    );

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "The check with your current security key failed.",
    );
    expect(onForbidden).not.toHaveBeenCalled();
  });

  it("tells the operator to use a different key when the browser refuses an excluded one", async () => {
    server.use(
      http.get(base, () =>
        HttpResponse.json({ credentials: [primary], max: 4 }),
      ),
      http.post(`${base}/stepup`, () => HttpResponse.json(stepUpOptions)),
      http.post(`${base}/register/begin`, () =>
        HttpResponse.json(creationOptions),
      ),
    );
    vi.spyOn(navigator.credentials, "get").mockResolvedValue(
      assertionCredential(),
    );
    vi.spyOn(navigator.credentials, "create").mockRejectedValue(
      new DOMException("excluded", "InvalidStateError"),
    );
    renderKeys();

    await userEvent.type(await screen.findByLabelText("New key name"), "Spare");
    await userEvent.click(
      screen.getByRole("button", { name: "Add a backup key" }),
    );

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "That security key is already registered.",
    );
  });

  it("ends the session when the list answers 401", async () => {
    server.use(http.get(base, () => new HttpResponse(null, { status: 401 })));
    const { onUnauthorized } = renderKeys();
    await waitFor(() => expect(onUnauthorized).toHaveBeenCalledTimes(1));
  });

  it.each([
    ["a credential ID", { ...primary, credential_id: "AAAA" }],
    ["a control character", { ...primary, name: "bad\u001bname" }],
    ["a malformed id", { ...primary, id: "not-a-uuid" }],
  ])("rejects a key carrying %s", async (_name, key) => {
    server.use(
      http.get(base, () => HttpResponse.json({ credentials: [key], max: 4 })),
    );
    renderKeys();
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "The server's response was not recognized.",
    );
    expect(screen.queryByText("YubiKey desk")).toBeNull();
  });
});
