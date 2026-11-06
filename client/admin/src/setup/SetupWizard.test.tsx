import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { afterEach, describe, expect, it } from "vitest";

import { server } from "../test/server";
import {
  buildAnswers,
  errorMessage,
  type SetupForm,
  SetupWizard,
} from "./SetupWizard";
import { parseInstall, parsePreflight, SetupError } from "./setupApi";

const TOKEN = "a".repeat(64);
const base = "http://localhost/api";

const passing = {
  schema: 1,
  ok: true,
  checks: [
    { name: "os", ok: true },
    { name: "ports", ok: true },
  ],
};

function agent(preflight: object = passing) {
  const seen: { auth: string[]; installs: unknown[] } = {
    auth: [],
    installs: [],
  };
  server.use(
    http.get(`${base}/preflight`, ({ request }) => {
      const auth = request.headers.get("Authorization") ?? "";
      seen.auth.push(auth);
      if (auth !== `Bearer ${TOKEN}`) {
        return HttpResponse.json({ error: "invalid_token" }, { status: 401 });
      }
      return HttpResponse.json(preflight);
    }),
    http.post(`${base}/install`, async ({ request }) => {
      seen.installs.push(await request.json());
      return HttpResponse.json({ ok: true, output: "next steps here" });
    }),
  );
  return seen;
}

afterEach(() => {
  globalThis.history.replaceState(null, "", "/");
});

const form: SetupForm = {
  domain: " example.com ",
  publicIp: "203.0.113.7",
  tlsMode: "letsencrypt",
  smtpHost: "smtp.example.com",
  smtpPort: "",
  smtpUser: "",
  smtpPass: "",
  smtpFrom: "",
  storageMode: "bundled",
  storageEndpoint: "ignored",
  storageRegion: "",
  storageBucket: "",
  storageAccessKey: "",
  storageSecretKey: "",
  storageUseSsl: true,
};

describe("buildAnswers", () => {
  it("sends required fields and leaves empty optional ones to the installer", () => {
    expect(buildAnswers(form)).toEqual({
      CFG_DOMAIN: "example.com",
      CFG_PUBLIC_IP: "203.0.113.7",
      CFG_TLS_MODE: "letsencrypt",
      CFG_SMTP_HOST: "smtp.example.com",
      CFG_STORAGE_MODE: "bundled",
    });
  });

  it("sends storage fields only for byo-s3", () => {
    expect(
      buildAnswers({
        ...form,
        smtpPass: " keep spaces ",
        storageMode: "byo-s3",
        storageEndpoint: "https://s3.example.com",
        storageAccessKey: "AK",
        storageSecretKey: "SK",
        storageUseSsl: false,
      }),
    ).toMatchObject({
      CFG_SMTP_PASS: " keep spaces ",
      CFG_STORAGE_MODE: "byo-s3",
      CFG_STORAGE_ENDPOINT: "https://s3.example.com",
      CFG_STORAGE_ACCESS_KEY: "AK",
      CFG_STORAGE_SECRET_KEY: "SK",
      CFG_STORAGE_USE_SSL: "false",
    });
  });
});

describe("setup API parsing", () => {
  it("rejects unexpected shapes", () => {
    expect(() => parsePreflight(null)).toThrow(SetupError);
    expect(() =>
      parsePreflight({ ok: true, checks: [{ name: 1, ok: true }] }),
    ).toThrow(SetupError);
    expect(() => parseInstall({ ok: true })).toThrow(SetupError);
    expect(parsePreflight({ ...passing, extra: 1 }).checks).toEqual(
      passing.checks,
    );
  });

  it("explains each refusal in plain words", () => {
    expect(errorMessage(new SetupError(401, "invalid_token"))).toMatch(
      /token is not right/,
    );
    expect(errorMessage(new SetupError(429, "too_many_attempts"))).toMatch(
      /Wait 10 minutes/,
    );
    expect(errorMessage(new SetupError(410, "setup_complete"))).toMatch(
      /already complete/,
    );
    expect(errorMessage(new SetupError(409, "install_running"))).toMatch(
      /already running/,
    );
    expect(errorMessage(new Error("x"))).toMatch(/could not complete/);
  });
});

describe("SetupWizard", () => {
  it("asks for the token and refuses a wrong one", async () => {
    const seen = agent();
    const user = userEvent.setup();
    render(<SetupWizard />);
    await user.type(screen.getByLabelText("Setup token"), "wrong");
    await user.click(screen.getByRole("button", { name: "Continue" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      /token is not right/,
    );
    expect(screen.getByLabelText("Setup token")).toBeInTheDocument();

    await user.clear(screen.getByLabelText("Setup token"));
    await user.type(screen.getByLabelText("Setup token"), TOKEN);
    await user.click(screen.getByRole("button", { name: "Continue" }));
    expect(
      await screen.findByText("Supported operating system"),
    ).toBeInTheDocument();
    expect(seen.auth).toEqual(["Bearer wrong", `Bearer ${TOKEN}`]);
  });

  it("takes the token from the link and removes it from the address", async () => {
    const seen = agent();
    globalThis.history.replaceState(null, "", `/#token=${TOKEN}`);
    render(<SetupWizard />);
    expect(
      await screen.findByText("The ports Concord Voice needs are free"),
    ).toBeInTheDocument();
    expect(globalThis.location.hash).toBe("");
    expect(seen.auth).toEqual([`Bearer ${TOKEN}`]);
  });

  it("blocks the install until every check passes", async () => {
    agent({
      schema: 1,
      ok: false,
      checks: [
        { name: "docker", ok: false },
        { name: "gpu", ok: true },
      ],
    });
    globalThis.history.replaceState(null, "", `/#token=${TOKEN}`);
    render(<SetupWizard />);
    expect(
      await screen.findByText("Docker and Docker Compose are installed"),
    ).toBeInTheDocument();
    expect(screen.getByText("gpu")).toBeInTheDocument();
    expect(screen.getByText("Failed")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Continue" })).toBeDisabled();
    expect(screen.getByText(/Fix the failed checks/)).toBeInTheDocument();
  });

  it("reports a lockout from the check-again button", async () => {
    agent();
    globalThis.history.replaceState(null, "", `/#token=${TOKEN}`);
    const user = userEvent.setup();
    render(<SetupWizard />);
    await screen.findByText("Supported operating system");
    server.use(
      http.get(`${base}/preflight`, () =>
        HttpResponse.json({ error: "too_many_attempts" }, { status: 429 }),
      ),
    );
    await user.click(screen.getByRole("button", { name: "Check again" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      /Wait 10 minutes/,
    );
  });

  async function reachAnswers() {
    const seen = agent();
    globalThis.history.replaceState(null, "", `/#token=${TOKEN}`);
    const user = userEvent.setup();
    render(<SetupWizard />);
    await screen.findByText("Supported operating system");
    await user.click(screen.getByRole("button", { name: "Continue" }));
    await user.type(screen.getByLabelText(/^Domain/), "example.com");
    await user.type(
      screen.getByLabelText("Public IPv4 address"),
      "203.0.113.7",
    );
    await user.click(screen.getByLabelText(/Self-signed/));
    await user.type(screen.getByLabelText("SMTP server"), "smtp.example.com");
    return { seen, user };
  }

  it("installs and shows the installer's next steps", async () => {
    const { seen, user } = await reachAnswers();
    await user.click(screen.getByRole("button", { name: "Install" }));
    expect(
      await screen.findByRole("heading", {
        name: "Concord Voice is configured",
      }),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Installer output")).toHaveValue(
      "next steps here",
    );
    expect(seen.installs).toEqual([
      {
        CFG_DOMAIN: "example.com",
        CFG_PUBLIC_IP: "203.0.113.7",
        CFG_TLS_MODE: "local",
        CFG_SMTP_HOST: "smtp.example.com",
        CFG_STORAGE_MODE: "bundled",
      },
    ]);
    expect(
      screen.queryByRole("button", { name: "Back to answers" }),
    ).not.toBeInTheDocument();
  });

  it("collects S3 storage details when chosen", async () => {
    const { seen, user } = await reachAnswers();
    await user.click(screen.getByLabelText("Use my own S3-compatible storage"));
    await user.type(
      screen.getByLabelText("Endpoint"),
      "https://s3.example.com",
    );
    await user.type(screen.getByLabelText("Access key"), "AK");
    await user.type(screen.getByLabelText("Secret key"), "SK");
    await user.click(screen.getByLabelText("Connect with HTTPS"));
    await user.click(screen.getByRole("button", { name: "Install" }));
    await screen.findByRole("heading", { name: "Concord Voice is configured" });
    expect(seen.installs[0]).toMatchObject({
      CFG_STORAGE_MODE: "byo-s3",
      CFG_STORAGE_ENDPOINT: "https://s3.example.com",
      CFG_STORAGE_USE_SSL: "false",
    });
  });

  it("keeps the answers after a failed install", async () => {
    const { user } = await reachAnswers();
    server.use(
      http.post(`${base}/install`, () =>
        HttpResponse.json({ ok: false, output: "installer: invalid domain" }),
      ),
    );
    await user.click(screen.getByRole("button", { name: "Install" }));
    expect(
      await screen.findByRole("heading", { name: "The installer stopped" }),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Installer output")).toHaveValue(
      "installer: invalid domain",
    );
    await user.click(screen.getByRole("button", { name: "Back to answers" }));
    expect(screen.getByLabelText(/^Domain/)).toHaveValue("example.com");
  });

  it("shows a refused install as an error", async () => {
    const { user } = await reachAnswers();
    server.use(
      http.post(`${base}/install`, () =>
        HttpResponse.json({ error: "install_running" }, { status: 409 }),
      ),
    );
    await user.click(screen.getByRole("button", { name: "Install" }));
    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent(/already running/),
    );
    expect(screen.getByRole("button", { name: "Install" })).toBeEnabled();
  });

  it("shows empty installer output plainly", async () => {
    const { user } = await reachAnswers();
    server.use(
      http.post(`${base}/install`, () =>
        HttpResponse.json({ ok: true, output: "" }),
      ),
    );
    await user.click(screen.getByRole("button", { name: "Install" }));
    expect(await screen.findByLabelText("Installer output")).toHaveValue(
      "No output.",
    );
  });
});
