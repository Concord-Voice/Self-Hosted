#!/usr/bin/env python3
# Heuristic anti-slop linter: the machine-checkable subset of ASD-STE100.
# Not a certified STE checker. The score delta between two drafts is the signal.
#
# Vendored from the "cure for AI slop" kit by Ege Celebi.
# Copyright (c) 2026 Ege Celebi. MIT licensed -- the full copyright and
# permission notice is in LICENSE.upstream beside this file, as MIT requires.
# https://github.com/woosal1337/blog/tree/b912d5fa59f368253683af2ebfac64ad6d08312d/videos/ep01-the-cure-for-ai-slop
#
# Local changes vs upstream (keep this list current; an upstream refresh should
# stay a small diff):
#   - this header
#   - encoding="utf-8" on open(), so the em-dash scan cannot crash under a
#     non-UTF-8 locale
#   - unwrap(): join hard-wrapped Markdown prose before sentence splitting, so a
#     wrapped long sentence is not read as two short ones
#   - CONTRACTION_S: match 's only after a known contraction stem, so ordinary
#     possessives ("the user's file") are not counted as contractions
#   - prose_sentences(): exclude list items from the paragraph-length rule, so a
#     vertical list is not flagged as an over-long paragraph
#   - strip_code(): also strip tilde-fenced blocks, so their identifiers are not
#     scored as prose
#
# FROZEN 2026-07-29. This reads Markdown with regular expressions, not a parser.
# Review found several further edge cases -- unclosed fences, a blank line inside
# a fence, a ``` run inside a `` span, pipe-less table rows, image sentence
# openers, stray backtick runs, four-space-indented blocks -- and each fix
# surfaced more without the set closing. They are documented in SKILL.md under
# "Known Markdown-parsing limits" and deliberately not fixed: the signal is the
# delta between a draft and its own rewrite, which a conforming parser does not
# improve. Do not reopen this by chasing one more construct; if the parsing model
# itself needs to change, replace the regexes with a real Markdown parser.
import re, sys, json, glob, os

MARKETING = ["seamless","seamlessly","robust","powerful","cutting-edge","effortless","effortlessly",
    "world-class","next-generation","revolutionary","blazing","lightning-fast","elegant","delightful",
    "turnkey","best-in-class","state-of-the-art","game-changing","first-class","battle-tested",
    "enterprise-grade","supercharge","unlock","unleash","empower","empowers"]
BANNED = ["begin","begins","commence","commences","initiate","initiates","originate",
    "utilize","utilizes","utilizing","leverage","leverages","leveraging","facilitate","facilitates",
    "ensure","ensures","ensuring","prior to","subsequent to","obtain","obtains","acquire","acquires",
    "demonstrate","demonstrates","additionally","furthermore","moreover","comprehensive","comprehensively",
    "utilization","aforementioned","henceforth","therein","whilst","amongst","numerous","myriad","plethora",
    "in order to","a variety of","in the event that","due to the fact that","it is important to note"]
PHRASAL = ["spin up","spin down","reach out","dive into","dives into","diving into","kick off","kicks off",
    "roll out","rolls out","tear down","ramp up","circle back","drill down","spun up","reaching out"]
MODAL_HEDGE = ["it is important to note","it should be noted","it is worth noting","please note that",
    "as mentioned","as noted above"]
# Stems whose 's is ALWAYS a contraction. Pronouns and indefinites that take a
# possessive ('s ("someone's file", "one's own", "nobody's business") are
# excluded -- including them reopens the possessive false positive this exists
# to close. The possessive of these stems is a different word (his, whose), so
# there is no ambiguous case left in the list.
CONTRACTION_S = r"\b(?:it|he|she|that|there|here|what|who|where|when|how|why|let)['’]s\b"
BE = r"(?:am|is|are|was|were|be|been|being)"
PP_IRREG = r"(?:done|made|sent|read|built|kept|held|set|put|run|written|shown|given|taken|found|got|gotten|seen|known|thrown|drawn)"

def strip_code(t):
    # Fenced blocks: either delimiter, any fence length. \2 captures the fence
    # character and \1 the whole opening run, so the closer must be the same
    # character and AT LEAST as long -- which is what Markdown permits, and it
    # still keeps a ```` block wrapping an inner ``` example from ending early.
    t = re.sub(r"(?ms)^[ \t]*((~|`)\2{2,})[^\n]*\n.*?^[ \t]*\1\2*[ \t]*$", " ", t)
    # Inline spans may use any run of backticks as the delimiter. Match the
    # longest runs first, or a ``double`` span leaves its code behind as prose.
    # This also subsumes a plain ```span```; a separate fixed ``` pattern used to
    # sit above it and could terminate early inside a longer run.
    t = re.sub(r"(`{2,})(?:(?!\1).)*?\1", " ", t, flags=re.S)
    t = re.sub(r"`[^`]*`", " ", t)
    return t

LIST_ITEM = r"^\s*(?:[-*+]|\d+[.)])\s+"
# Stands alone: never absorbs a following wrapped line. A list item is NOT here
# -- it starts a logical line that its indented continuations join. A blockquote
# is not here either: a hard-wrapped callout marks EVERY line with ">", so
# treating each as standalone would exempt quoted runbook and safety text from
# the length rule. Its marker is stripped and its lines join instead.
STANDALONE = re.compile(r"^\s*(?:#{1,6}\s|[|]|```|~~~)")
QUOTE_MARKER = r"^\s*>\s?"
# Sentence boundary. "(" is deliberately NOT a sentence opener, and there is no
# abbreviation guard. Measured against four cases -- "e.g. (" mid-sentence, a
# standalone parenthetical sentence, "etc." genuinely ending a sentence, and a
# capitalized "E.g." -- this plain form is correct on three. Admitting "(" is
# correct on two, and adding fixed-width abbreviation lookbehinds is also
# correct on two, because a guard that suppresses "e.g." mid-sentence equally
# suppresses "etc." when it really does end one. That circularity has no regex
# answer, so the plain form stands and the residue is a documented limit. Its
# one failure -- a standalone parenthetical merging into the sentence before it
# -- is a false positive, which is the safe direction for a linter.
SENTENCE_SPLIT = r"(?<=[.!?])\s+(?=[A-Z0-9\"'\-*_\[`])"
# Thematic break: three or more of the same -, *, or _ with optional spaces,
# and nothing else on the line. Checked BEFORE LIST_ITEM, which it also matches.
THEMATIC = re.compile(r"^[ \t]*(?:(?:\*[ \t]*){3,}|(?:-[ \t]*){3,}|(?:_[ \t]*){3,})$")

def unwrap(text):
    # Markdown hard-wraps prose, so one sentence often spans several physical
    # lines. Join contiguous lines into one logical line before sentence
    # splitting, or a wrapped 21-word sentence reads as two short ones and
    # escapes the length rule.
    out, buf = [], []
    def flush():
        if buf: out.append(" ".join(buf)); buf.clear()
    for line in text.split("\n"):
        # Strip a blockquote marker so a wrapped callout joins as one sentence.
        line = re.sub(QUOTE_MARKER, "", line)
        s = line.strip()
        if not s:
            flush(); out.append(line)
        elif THEMATIC.match(line):
            # A thematic break ("* * *", "---") matches LIST_ITEM but is not a
            # list. Left to the branch below it would absorb the following
            # paragraph, and prose_sentences() would then drop that whole
            # paragraph as a list item -- exempting it from the length rule.
            # Emit a blank line, not the break itself: the boundary is what
            # matters, and passing the separator through made it count as a
            # seventh sentence and fail a valid six-sentence paragraph.
            flush(); out.append("")
        elif re.match(LIST_ITEM, line):
            # A marker begins a new logical line and absorbs its continuations,
            # so a hard-wrapped numbered step is still measured whole. Flushing
            # it standalone would exempt the exact format SKILL.md recommends.
            flush(); buf.append(s)
        elif STANDALONE.match(line):
            flush(); out.append(line)
        else:
            buf.append(s)
    flush()
    return "\n".join(out)

def sentences(text):
    out = []
    for line in unwrap(text).split("\n"):
        s = line.strip()
        if not s: continue
        s = re.sub(r"^\s*#{1,6}\s*", "", s)
        s = re.sub(LIST_ITEM, "", s)
        if not s: continue
        # Split on . ! ? only. A colon usually introduces a clause or a list
        # inside one sentence ("uses three services: Redis ..."), so treating it
        # as a boundary chopped long sentences into passing fragments.
        # The lookahead admits Markdown emphasis and link openers, or a
        # sentence starting **like this** merges into the previous one and the
        # pair reports as one over-long sentence. See SENTENCE_SPLIT for why
        # "(" is excluded and why no abbreviation guard is used.
        parts = re.split(SENTENCE_SPLIT, s)
        for p in parts:
            p = p.strip()
            if p: out.append(p)
    return out

def prose_sentences(paragraph):
    # Paragraph-length rule only. A vertical list is a structure, not a
    # paragraph, and SKILL.md recommends vertical lists for steps -- counting
    # each item as a sentence flags the exact shape the skill asks for.
    # Unwrap FIRST so a wrapped item's continuation is already folded into its
    # marker line and drops out with it; filtering raw lines would leave the
    # continuation behind to be counted as prose.
    lines = [l for l in unwrap(paragraph).split("\n") if l.strip() and not re.match(LIST_ITEM, l)]
    return sentences(strip_code("\n".join(lines)))

def wc(s):
    return len([w for w in re.findall(r"[A-Za-z0-9][A-Za-z0-9'\-/]*", s)])

def count_ci(text, phrases):
    n = 0; hits = []
    low = text.lower()
    for ph in phrases:
        for m in re.finditer(r"(?<![a-z])" + re.escape(ph) + r"(?![a-z])", low):
            n += 1; hits.append(ph)
    return n, hits

def lint(text):
    raw = text
    text = strip_code(text)
    sents = sentences(text)
    words = sum(wc(s) for s in sents) or 1
    v = {}
    longs = [(wc(s), s) for s in sents if wc(s) > 20]
    v["long_sentence(>20w)"] = len(longs)
    v["semicolon"] = text.count(";")
    # 's is ambiguous: "it's" is a contraction, "the user's file" is a possessive.
    # Match 's only after a known contraction stem, or every possessive in
    # ordinary technical prose scores as a violation.
    v["contraction"] = (len(re.findall(r"\b\w+['’](?:t|re|ve|ll|d|m)\b", text))
                        + len(re.findall(CONTRACTION_S, text, re.I)))
    v["passive_voice"] = len(re.findall(rf"\b{BE}\s+(?:\w+ed|{PP_IRREG})\b", text, re.I))
    v["ing_main_verb"] = len(re.findall(rf"\b{BE}\s+\w+ing\b", text, re.I))
    v["nominalization"] = len(re.findall(r"\b(?:perform(?:s|ed)?|conduct(?:s|ed)?|provide(?:s|d)?|carry out|carries out|make use of|makes use of)\b", text, re.I)) + len(re.findall(r"\b\w{4,}(?:tion|ment|ance|ence)\s+of\b", text, re.I))
    v["phrasal_verb"], _ = count_ci(text, PHRASAL)
    v["banned_word"], bh = count_ci(text, BANNED)
    v["marketing_adjective"], mh = count_ci(text, MARKETING)
    v["modal_hedge"], _ = count_ci(text, MODAL_HEDGE)
    paras = [p for p in re.split(r"\n\s*\n", raw) if p.strip()]
    v["long_paragraph(>6s)"] = sum(1 for p in paras if len(prose_sentences(p)) > 6)
    em = raw.count("—") + raw.count("–")
    total = sum(v.values())
    per100 = {k: round(x*100.0/words, 2) for k, x in v.items()}
    return {
        "words": words, "sentences": len(sents),
        "violations": v, "total": total,
        "total_per100w": round(total*100.0/words, 2),
        "em_dash(slop-marker)": em,
        "longest_sentence_words": (max(longs)[0] if longs else max((wc(s) for s in sents), default=0)),
        "sample_marketing": list(dict.fromkeys(mh))[:6],
        "sample_banned": list(dict.fromkeys(bh))[:6],
    }

if __name__ == "__main__":
    files = sys.argv[1:] or []
    if not files:
        print(json.dumps(lint(sys.stdin.read()), indent=2)); sys.exit(0)
    exp = []
    for f in files: exp += sorted(glob.glob(f)) if any(c in f for c in "*?[") else [f]
    for f in exp:
        with open(f, encoding="utf-8") as fh: r = lint(fh.read())
        print(f"{os.path.basename(f):32} words={r['words']:4d} total={r['total']:3d} per100w={r['total_per100w']:6.2f} em_dash={r['em_dash(slop-marker)']:2d}")
