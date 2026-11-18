---
description: Lines that hold an [internal] placeholder stay byte-for-byte unchanged
paths:
  - "client/admin/**"
  - "docs/api/**"
  - "infrastructure/deploy/**"
  - "infrastructure/docker/**"
  - "services/**"
---

# `[internal]` placeholder lines

Some lines under these paths hold an `[internal]` placeholder where text was
removed before publication. A downstream sync imports every change to these
paths, and it rejects a change whose added lines hold `[internal]`: importing
such a line would overwrite the real downstream text with the placeholder. One
rewritten line blocks every later change until it is fixed here.

- Before you edit a line, check it for `[internal]`. Leave such a line
  byte-for-byte unchanged, even to fix a typo in it.
- Put new text on a new line next to it. In JSON, put it under a new key.
- Commit, then run this before you push. It checks the commits you push, from
  any directory, and must print nothing:

  ```bash
  git fetch -q origin main
  git diff --no-color --no-ext-diff --no-textconv --no-relative -U0 origin/main...HEAD -- \
      ':(top)client/admin' ':(top)docs/api' ':(top)infrastructure/deploy' ':(top)infrastructure/docker' ':(top)services' \
    | awk '/^diff --git /{h=0} /^@@/{h=1; next} h && /^[-+]/' | grep -F '[internal]'
  ```

  It prints any placeholder line you added, changed or deleted.

  The one exception is a fix that restores a rewritten line to its earlier
  text. Leave that file out of the check above (add `':(top,exclude)<file>'`
  after the paths), and diff the file alone against the last commit before its
  line was first rewritten; that must print nothing.

It has happened at least four times: #210 and #224, then #331, which rewrote a
`"//"` comment in `services/media-plane/.audit-allowlist.json` (fixed in #358),
and #338, which rewrote the squawk item in
`services/control-plane/migrations/README.md` (fixed in #363).
