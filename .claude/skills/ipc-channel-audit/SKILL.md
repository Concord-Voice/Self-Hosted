---
name: ipc-channel-audit
description: Enumerate every Electron IPC channel surface in the Electron app dir, cross-reference renderer-side callers, and verify each main-process handler validates the sender frame. Prints a table of channels and flags any handler missing sender validation. Use before shipping a release or when adding IPC surface.
argument-hint: "[--fix] [--only-new]"
allowed-tools:
  - Bash(grep *)
  - Bash(rg *)
  - Bash(find *)
  - Bash(git diff *)
  - Read
  - Grep
  - Glob
---

# IPC Channel Audit

Audit the Electron IPC surface for completeness and security per
`.claude/rules/electron.md`. This skill is read-only by default; pass
`--fix` to have Claude propose patches for any gaps found.

> Paths below assume the Electron app lives at the repo root (`.`) — the
> directory named by `rc_get '.paths.electron_dir'` (default `.`). If this
> repo nests it elsewhere (e.g. `apps/desktop/`), prefix every path below
> with that directory instead.

Args: $0
  - `--fix` — propose edits for each missing sender validation (still requires user approval per file)
  - `--only-new` — audit only channels added since the last commit on `main`

## Background (Read First)

`.claude/rules/electron.md` says:
- All renderer→main IPC goes through `src/preload/` via `contextBridge.exposeInMainWorld`
- Main-process handlers MUST validate `event.senderFrame.url` before processing
- No `@electron/remote`, no `nodeIntegration`, sandbox ON, contextIsolation ON
- Expose minimal API surface — only what the renderer strictly needs

The two places that matter:
- `src/preload/` — `contextBridge.exposeInMainWorld(...)` calls define the exposed surface
- `src/main/ipc/` (and any `ipcMain.handle`/`ipcMain.on` elsewhere in `src/main/`)

## Audit Steps

### Step 1: Enumerate exposed surface (preload side)
```bash
rg -n "contextBridge\.exposeInMainWorld" src/preload/
rg -n "ipcRenderer\.(invoke|send)" src/preload/
```
Build a list of channel names. These are the *only* channels the renderer
can legitimately call.

### Step 2: Enumerate main-process handlers
```bash
rg -n "ipcMain\.(handle|on)" src/main/
```
Build a list of channels that have handlers, and the file:line of each.

### Step 3: Cross-reference
Compare the two lists:
- **Preload exposes but main has no handler** → dead channel (LOW, cleanup)
- **Main handles but preload does not expose** → leaked handler reachable only by misconfiguration (MEDIUM — flag, may be used by tests)
- **Both present** → valid channel, continue to Step 4

### Step 4: Sender validation check
For each valid channel, read the handler body and verify it calls something
equivalent to:
```ts
if (!isAllowedSender(event.senderFrame.url)) {
  throw new Error('Unauthorized IPC sender');
}
```
or uses a shared middleware/wrapper that performs the check.

Flag HIGH if:
- No `event.senderFrame.url` reference in the handler function body
- The handler directly accesses `event.sender` without checking the frame URL
- The handler imports `@electron/remote` (should never happen)

Flag CRITICAL if:
- The handler executes shell commands, spawns child processes, or touches the
  filesystem without first validating the sender
- The handler exposes `safeStorage`, tokens, or encryption keys without
  sender validation

### Step 5 (optional, `--fix`): Propose patches
For each HIGH/CRITICAL finding, draft an Edit that adds a sender check at
the top of the handler. Show the user the diff and wait for approval on
each file. Do NOT batch-apply.

### Step 6 (optional, `--only-new`): Restrict to new surface
If `--only-new` is set, first run:
```bash
git diff main...HEAD -- src/preload/ src/main/
```
and audit only channels introduced or modified in that diff. This is the
fast path for per-PR review.

## Output Format

### Channels table
```
IPC Channel Audit — Electron app
─────────────────────────────────────────────────────────────────────────
Channel                              Preload   Main Handler             Sender Check
─────────────────────────────────────────────────────────────────────────
auth:login                           ✓         src/main/ipc/auth.ts:12  ✓
auth:logout                          ✓         src/main/ipc/auth.ts:34  ✓
storage:getSecret                    ✓         src/main/ipc/storage.ts:8 ✗ CRITICAL
window:minimize                      ✓         src/main/ipc/window.ts:5 ✗ LOW (UI op)
updater:checkForUpdates              ✓         src/main/ipc/updater.ts:11 ✓
machineId:get                        ✓         src/main/ipc/machine.ts:7 ✓
ipc:devtools                         ✗         src/main/ipc/debug.ts:3  — (dead handler)
─────────────────────────────────────────────────────────────────────────
Total: 7 channels, 1 CRITICAL, 1 LOW, 1 dead
```

### Findings
For each non-✓ row, print:
- `file:line` of the handler
- the rule from `.claude/rules/electron.md`
- severity and fix

## Severity Scale

- **CRITICAL** — handler touches secrets, filesystem, or shell without sender validation. Blocks merge.
- **HIGH** — handler exposes app state or triggers side effects without sender validation.
- **MEDIUM** — handler in main but no preload binding (reachable via misconfig or tests).
- **LOW** — harmless UI-only operation (e.g., window minimize) without explicit sender check. Optional to fix.

## What This Skill Does NOT Do

- Does not audit `@electron/remote` usage — already forbidden project-wide, pre-commit hooks catch it.
- Does not enforce `contextIsolation`/`sandbox`/`nodeIntegration` flags — those are set in `BrowserWindow` creation, not per-channel, and the existing `protect-sensitive.sh` hook guards the relevant files.
- Does not run Playwright E2E to validate channels dynamically — this is a static audit.

## Why This Skill Exists

Electron IPC is often the largest attack surface on an Electron app's client side. The
rules exist, but enforcing them requires knowing the *complete* set of
channels, and that set drifts as features ship. A per-release audit catches
drift that editor LSP and linting miss. Running `--only-new` on every PR
keeps new IPC surface clean without needing a full sweep each time.
