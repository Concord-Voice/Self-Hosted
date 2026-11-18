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
- Before you push, this must print nothing:

  ```bash
  git diff -U0 origin/main -- client/admin docs/api infrastructure/deploy infrastructure/docker services | grep '^+' | grep -F '[internal]'
  ```

  The one exception is a fix that restores a rewritten line to its earlier
  text: diff against the commit before the rewrite instead.

It has happened twice: #331 rewrote a `"//"` comment in
`services/media-plane/.audit-allowlist.json` (fixed in #358), and #338 rewrote
the squawk item in `services/control-plane/migrations/README.md`.
