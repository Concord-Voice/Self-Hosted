---
name: ste-writing
description: Rewrite prose (docs, READMEs, PR descriptions, error messages, release notes, comments — never code) into ASD-STE100 Simplified Technical English to remove "AI slop". Use when explicitly asked to make writing not sound like AI, to make text clear or plain, to enforce a controlled writing style, or to draft new technical documentation that reads human. Two modes — strict (procedures, safety, error messages) and STE-flavored (general prose). Opt-in only; never apply unprompted to prose that already has an author's voice.
argument-hint: "[strict|flavored] [path-or-text]"
allowed-tools:
  - Read
  - Write
  - Edit
  - Grep
  - Glob
  - Bash(python3 .claude/skills/ste-writing/ste-lint.py *)
---

# ste-writing

Write prose in ASD-STE100 Simplified Technical English. This applies to documentation, READMEs, pull-request text, error messages, release notes, and comments. It does not apply to code, identifiers, or command syntax. It is not for marketing copy, essays, or anything that needs a voice — STE strips voice on purpose.

## Scope

This skill is **opt-in**. Run it when the developer asks for it. Do not apply it as a background pass on prose that already has an author's voice.

Do not rewrite these unless asked to by name:

- `CLAUDE.md`, `AGENTS.md`, `GEMINI.md`, `.github/copilot-instructions.md`
- `.claude/rules/**`, `.claude/skills/**`, `docs/adr/**`, `docs/policies/**`

Those files are hand-authored and carry intentional voice. Where the repository has a documentation agent that owns voice calibration, those files belong to that agent. STE is the right tool for **new** operator-facing text — runbook steps, error strings, migration notes, release notes, install docs — and for text the developer has asked to de-slop.

## Rules

WORDS

- Use one name for one thing. Do not call the same item by two different names.
- Use the short common word: start (not begin/commence/initiate), use (not utilize/leverage), help (not facilitate), make sure (not ensure), before (not prior to), after (not subsequent to), about (not regarding/concerning), get (not obtain/acquire), show (not demonstrate), also (not additionally/furthermore/moreover).
- Give each word one meaning. "fall" means to move down, not to decrease.
- No marketing adjectives: seamless, robust, powerful, cutting-edge, effortless, world-class, next-generation, revolutionary.
- American spelling.

VERBS

- Active voice. "the parser reads the file", not "the file is read by the parser".
- Use a verb for an action. "analyze the log", not "perform an analysis of the log".
- No stacked auxiliaries. Not "it is important to note that this may help to improve". Write "this improves X".
- No "-ing" main verb where a simple tense works.

SENTENCES

- One instruction per sentence. Max 20 words for an instruction. ASD-STE100 allows 25 for a descriptive sentence, but the linter applies a single 20-word threshold to every sentence, because it cannot tell the two kinds apart. Treat a flagged 21-to-25-word descriptive sentence as advisory.
- No contractions. Use articles: a, an, the, this, these.

PUNCTUATION

- No semicolons. Write two sentences. The em dash is not banned by ASD-STE100. The linter counts em dashes as a separate slop marker and leaves them out of the score, so ban them yourself if you want them gone.

STRUCTURE

- One topic per paragraph, max six sentences. For steps, use a numbered vertical list, one action per item, imperative form. Put a condition before its command.

Write only the requested text. No preamble, no summary, no closing remarks.

## Modes

- **strict** — procedures, runbooks, safety text, error messages. Apply every rule and both length caps.
- **STE-flavored** (default) — general prose such as READMEs, PR descriptions, and docs. Apply the sentence, paragraph, active-voice, and no-phrasal-verb discipline. Relax the ~900-word dictionary lockdown so the text keeps enough range to read naturally.

## Self-lint

Check these before you return text:

1. Any sentence over 20 words? Split it.
2. Any semicolon? Replace it with a period.
3. Any contraction? Expand it.
4. Any passive voice with a known actor? Make it active.
5. Any "-ing" main verb, nominalization ("perform an analysis"), or phrasal verb ("spin up")? Replace it with a plain verb.
6. Same thing named two ways? Pick one name.

Then run the linter for the mechanical subset:

```bash
python3 .claude/skills/ste-writing/ste-lint.py path/to/draft.md
```

The score is violations per 100 words. Lower is cleaner. Lint the draft, apply the skill, then lint it again. The delta between the two scores is the signal, not the absolute number. The linter also reads stdin when you give it no file.

## Limits

The mechanical rules above are lintable, and they are what removes slop. Full ASD-STE100 also needs human judgment — the right technical noun, and whether a sentence makes good sense. A checker cannot certify that. This skill fixes the FORM of slop. It cannot make a hollow paragraph true.

The linter is a heuristic, not a certified STE checker. Treat a flagged line as a prompt to look, not as a defect.

### Known Markdown-parsing limits

The linter reads Markdown with regular expressions, not a parser. It handles the constructs that appear in ordinary technical prose, and these known cases are out of scope. **The list is frozen deliberately.** Review found each of them real, and each fix surfaced further edge cases without the set ever closing; a heuristic scorer does not need a conforming parser to be useful, because the signal is the delta between a draft and its own rewrite, not the absolute number.

- **Four-space-indented code blocks** are scored as prose. Fence your code instead. Stripping every four-space-indented line would also remove indented list continuations, hiding over-long wrapped instructions — a worse error than the one it fixes.
- **Unclosed fences** are not stripped through end-of-document. A fence with no closing delimiter leaves its contents scored as prose.
- **A fenced block containing a blank line** is split into fragments by the paragraph rule before the fence is stripped.
- **A triple-backtick run inside a double-backtick span** ends the span early, leaving the tail as prose.
- **Tables written without outer pipes** (`Name | Description`) are read as prose rather than table rows.
- **Image openers** (`![alt](src)`) are not recognized as sentence starts, so an image-led sentence merges into the previous one.
- **Stray unbalanced backtick runs** in prose can swallow the text between them, since a stray run is indistinguishable from an opening delimiter.
- **A standalone parenthetical sentence** merges into the sentence before it, because `(` is not treated as a sentence opener. Admitting `(` would instead split `e.g. (the stored token)` at the abbreviation and let a long sentence escape the length check — a worse, silent failure. An abbreviation guard does not resolve this: suppressing the split after `e.g.` equally suppresses it after `etc.` when that genuinely ends a sentence, and the two are indistinguishable without knowing where the sentence ends. Measured across those cases, the current plain form is correct more often than either alternative, and its failure is a visible false positive rather than a silent miss.

If a document trips one of these, fence the code or rephrase; do not treat the score as authoritative for that file.

The linter cannot tell a used word from a quoted one. Any document that names the words it bans scores badly against itself. This file does, and scores under 5 per 100 words for that reason. Compare a draft against its own rewrite, not against another document.

Free official standard (do not paste it in full; it is copyrighted): https://asd-ste100.org

## Attribution

Adapted from the "cure for AI slop" kit by Ege Çelebi. Copyright (c) 2026 Ege Çelebi, MIT licensed:
<https://github.com/woosal1337/blog/tree/b912d5fa59f368253683af2ebfac64ad6d08312d/videos/ep01-the-cure-for-ai-slop>

The full copyright and permission notice is in `LICENSE.upstream` beside this file, as the MIT license requires for copies and substantial portions. Local changes to the linter are listed in its header.

### First use in a new repository

This is repository policy, not a term of the upstream license.

`LICENSE.upstream` travels with the skill directory, so a repository that receives the skill by sync already carries the required notice and owes nothing further to hold a copy.

The separate obligation is an entry in that repository's own NOTICE file, and it is triggered by **use**, not by possession. The first time this skill is invoked in a repository that has not used it before — the first time its output contributes to that repository's content — add an entry for the kit to that repository's NOTICE file. Create one if the repository publishes artifacts and has none. A repository holding an unused copy owes no entry.

The kit is vendored source, not a managed package, so it never appears in a regenerated dependency listing. Add the entry by hand.

A repository that has invoked the skill carries its entry under a heading such as "Build-Time and Development Dependencies" in `NOTICE.md`.

The author's own cross-model measurement reported the STE skill cutting the linter score by 74% on Claude Sonnet and 50% on gpt-5.5 against an unguided baseline. It was the best of the four conditions on Claude Sonnet. On gpt-5.5 it did not lead: Orwell's six rules scored 1.69 against STE's 1.76, which is why the author's own summary claims only "best or tied-best". Every writing system tested beat the unguided baseline; a banned-words list was the weakest. Those numbers are the author's, measured with this linter, and are not independently reproduced here.
