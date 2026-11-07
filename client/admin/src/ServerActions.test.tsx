import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it, vi } from "vitest";

import { parseServerBackups, parseServerJob, parseServerJobs } from "./server";
import { ServerActions } from "./ServerActions";
import { server } from "./test/server";

const base = "http://localhost/admin/api/v1/server";
const id = "0123456789abcdef0123456789abcdef";
const archive = "concord-backup-20261105T010203Z.tar.gz";
const started = "2026-11-07T06:00:00Z";

const job = (state: string, extra: Record<string, unknown> = {}) => ({
  id,
  action: "backup",
  state,
  started,
  ...extra,
});

function lists(jobs: unknown[] = [], backups: unknown[] = []) {
  server.use(
    http.get(`${base}/jobs`, () => HttpResponse.json({ jobs })),
    http.get(`${base}/backups`, () => HttpResponse.json({ backups })),
  );
}

function renderActions() {
  const onSessionError = vi.fn(() => "session error");
  render(<ServerActions onSessionError={onSessionError} pollMs={10} />);
  return { onSessionError };
}

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

const stepUpOptions = {
  handle: "h".repeat(64),
  publicKey: {
    publicKey: {
      challenge: "AQID",
      rpId: "localhost",
      allowCredentials: [{ type: "public-key", id: "BAUG" }],
      userVerification: "required",
    },
  },
};

describe("ServerActions", () => {
  it("backs up without a security key and follows the job", async () => {
    lists();
    const bodies: unknown[] = [];
    let polls = 0;
    server.use(
      http.post(`${base}/actions/backup`, async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json(job("running"), { status: 202 });
      }),
      http.get(`${base}/jobs/${id}`, () => {
        polls += 1;
        return HttpResponse.json(
          polls < 2
            ? job("running", { output: "dumping\n" })
            : job("succeeded", {
                output: "done\n",
                archive,
                finished: started,
              }),
        );
      }),
    );
    const get = vi.spyOn(navigator.credentials, "get");
    renderActions();
    await userEvent
      .setup()
      .click(await screen.findByRole("button", { name: "Back up now" }));
    expect(
      await screen.findByText(
        `Back up: succeeded, wrote ${archive}`,
        {},
        { timeout: 4000 },
      ),
    ).toBeVisible();
    expect(screen.getByRole("textbox", { name: "Back up output" })).toHaveValue(
      "done\n",
    );
    expect(bodies).toEqual([{}]);
    expect(get).not.toHaveBeenCalled();
  });

  it("locks the buttons while its own job runs and rides out a rate limit", async () => {
    lists();
    let polls = 0;
    server.use(
      http.post(`${base}/actions/backup`, () =>
        HttpResponse.json(job("running"), { status: 202 }),
      ),
      http.get(`${base}/jobs/${id}`, () => {
        polls += 1;
        if (polls === 1) return HttpResponse.json(job("running"));
        if (polls === 2)
          return HttpResponse.json({ error: "rate_limited" }, { status: 429 });
        return HttpResponse.json(job("succeeded", { finished: started }));
      }),
    );
    const { onSessionError } = renderActions();
    await userEvent
      .setup()
      .click(await screen.findByRole("button", { name: "Back up now" }));
    expect(await screen.findByText("Back up: running")).toBeVisible();
    expect(screen.getByRole("button", { name: "Start" })).toBeDisabled();
    expect(await screen.findByText("Back up: succeeded")).toBeVisible();
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Start" })).toBeEnabled(),
    );
    expect(onSessionError).not.toHaveBeenCalled();
  });

  it("still loads jobs when the backups folder is untrusted", async () => {
    server.use(
      http.get(`${base}/jobs`, () =>
        HttpResponse.json({ jobs: [job("running")] }),
      ),
      http.get(`${base}/backups`, () =>
        HttpResponse.json({ error: "untrusted_backups" }, { status: 409 }),
      ),
      http.get(`${base}/jobs/${id}`, () => HttpResponse.json(job("running"))),
    );
    renderActions();
    expect(
      await screen.findByText(/backups folder is not owned by root/),
    ).toBeVisible();
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Start" })).toBeDisabled(),
    );
  });

  it("notices a job another admin starts later", async () => {
    let calls = 0;
    server.use(
      http.get(`${base}/jobs`, () => {
        calls += 1;
        return HttpResponse.json({ jobs: calls > 1 ? [job("running")] : [] });
      }),
      http.get(`${base}/backups`, () => HttpResponse.json({ backups: [] })),
      http.get(`${base}/jobs/${id}`, () => HttpResponse.json(job("running"))),
    );
    renderActions();
    const start = await screen.findByRole("button", { name: "Start" });
    expect(start).toBeEnabled();
    await waitFor(() => expect(start).toBeDisabled());
  });

  it("turns read-only when a write finds an appliance", async () => {
    lists();
    server.use(
      http.post(`${base}/actions/start`, () =>
        HttpResponse.json(
          { error: "appliance_managed", appliance: "truenas" },
          { status: 409 },
        ),
      ),
    );
    renderActions();
    await userEvent
      .setup()
      .click(await screen.findByRole("button", { name: "Start" }));
    expect(
      await screen.findByText(/TrueNAS runs this server's containers/),
    ).toBeVisible();
  });

  it("asks for the security key before a restore and sends the assertion", async () => {
    lists([], [{ name: archive, size: 3 << 20, modified: started }]);
    let stepUpBody: unknown;
    let actionBody: Record<string, unknown> = {};
    server.use(
      http.post(`${base}/stepup`, async ({ request }) => {
        stepUpBody = await request.json();
        return HttpResponse.json(stepUpOptions);
      }),
      http.post(`${base}/actions/restore`, async ({ request }) => {
        actionBody = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json(job("running", { action: "restore" }), {
          status: 202,
        });
      }),
      http.get(`${base}/jobs/${id}`, () =>
        HttpResponse.json(job("failed", { action: "restore", output: "no\n" })),
      ),
    );
    vi.spyOn(navigator.credentials, "get").mockResolvedValue(
      assertionCredential(),
    );
    renderActions();
    const restore = await screen.findByRole("button", { name: "Restore" });
    await waitFor(() => expect(restore).toBeEnabled());
    await userEvent.setup().click(restore);
    expect(
      await screen.findByText("The action failed. Its output is below."),
    ).toBeVisible();
    expect(stepUpBody).toEqual({ action: "restore" });
    expect(actionBody.archive).toBe(archive);
    expect((actionBody.stepup as { handle: string }).handle).toBe(
      stepUpOptions.handle,
    );
  });

  it("explains a refused step-up without ending the session", async () => {
    lists();
    server.use(
      http.post(`${base}/stepup`, () => HttpResponse.json(stepUpOptions)),
      http.post(`${base}/actions/stop`, () =>
        HttpResponse.json({ error: "stepup_failed" }, { status: 403 }),
      ),
    );
    vi.spyOn(navigator.credentials, "get").mockResolvedValue(
      assertionCredential(),
    );
    const { onSessionError } = renderActions();
    await userEvent
      .setup()
      .click(await screen.findByRole("button", { name: "Stop" }));
    expect(
      await screen.findByText("The security key check failed. Try again."),
    ).toBeVisible();
    expect(onSessionError).not.toHaveBeenCalled();
  });

  it("reports a canceled security key", async () => {
    lists();
    server.use(
      http.post(`${base}/stepup`, () => HttpResponse.json(stepUpOptions)),
    );
    vi.spyOn(navigator.credentials, "get").mockRejectedValue(
      new DOMException("no", "NotAllowedError"),
    );
    renderActions();
    await userEvent
      .setup()
      .click(await screen.findByRole("button", { name: "Restart" }));
    expect(
      await screen.findByText("Security key verification was canceled."),
    ).toBeVisible();
  });

  it("passes other errors to the session handler", async () => {
    lists();
    server.use(
      http.post(`${base}/actions/start`, () =>
        HttpResponse.json({}, { status: 401 }),
      ),
    );
    const { onSessionError } = renderActions();
    await userEvent
      .setup()
      .click(await screen.findByRole("button", { name: "Start" }));
    await waitFor(() => expect(onSessionError).toHaveBeenCalled());
    expect(await screen.findByText("session error")).toBeVisible();
  });

  it("only enables update for a release tag", async () => {
    lists();
    renderActions();
    const user = userEvent.setup();
    const update = await screen.findByRole("button", { name: "Update" });
    expect(update).toBeDisabled();
    await user.type(
      screen.getByRole("textbox", { name: "Update to release tag" }),
      "main",
    );
    expect(update).toBeDisabled();
    await user.clear(
      screen.getByRole("textbox", { name: "Update to release tag" }),
    );
    await user.type(
      screen.getByRole("textbox", { name: "Update to release tag" }),
      "v0.3.1",
    );
    expect(update).toBeEnabled();
  });

  it("locks the buttons while a job runs elsewhere", async () => {
    lists([job("running")]);
    server.use(
      http.get(`${base}/jobs/${id}`, () => HttpResponse.json(job("running"))),
    );
    renderActions();
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Start" })).toBeDisabled(),
    );
    expect(await screen.findByText("Back up: running")).toBeVisible();
  });

  it("is read-only on an appliance", async () => {
    server.use(
      http.get(`${base}/jobs`, () =>
        HttpResponse.json(
          { error: "appliance_managed", appliance: "truenas" },
          { status: 409 },
        ),
      ),
      http.get(`${base}/backups`, () =>
        HttpResponse.json(
          { error: "appliance_managed", appliance: "truenas" },
          { status: 409 },
        ),
      ),
    );
    renderActions();
    expect(
      await screen.findByText(/TrueNAS runs this server's containers/),
    ).toBeVisible();
    expect(screen.queryByRole("button", { name: "Start" })).toBeNull();
  });
});

describe("server write parsers", () => {
  it("accepts the agent's shapes", () => {
    expect(
      parseServerJob(job("succeeded", { archive, finished: started })).archive,
    ).toBe(archive);
    expect(parseServerJobs({ jobs: [job("running")] })).toHaveLength(1);
    expect(
      parseServerBackups({
        backups: [{ name: archive, size: 1, modified: started }],
      }),
    ).toHaveLength(1);
  });

  it.each([
    job("running", { id: "x" }),
    job("running", { action: "rm" }),
    job("paused"),
    job("running", { started: "soon" }),
    job("running", { archive: "../etc" }),
    job("running", { output: 1 }),
    null,
  ])("rejects a bad job %#", (value) => {
    expect(() => parseServerJob(value)).toThrow();
  });

  it("rejects bad backups", () => {
    expect(() =>
      parseServerBackups({
        backups: [{ name: "x", size: 1, modified: started }],
      }),
    ).toThrow();
    for (const size of [1.5, -1]) {
      expect(() =>
        parseServerBackups({
          backups: [{ name: archive, size, modified: started }],
        }),
      ).toThrow();
    }
    expect(() => parseServerBackups({})).toThrow();
    expect(() => parseServerJobs({})).toThrow();
  });
});
