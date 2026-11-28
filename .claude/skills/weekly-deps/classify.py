"""Classify verified dependency evidence without consulting merge readiness."""
import argparse
from fnmatch import fnmatchcase
import json
import re
import sys


BUMP_RANK = {"patch": 0, "minor": 1, "major": 2}
LIST_FIELDS = ("groups", "packages", "ecosystems", "directories")
RULE_FIELDS = {*LIST_FIELDS, "ungrouped", "dependency_type", "max_bump"}


def matches(values, patterns):
    return bool(values) and all(any(fnmatchcase(value, pattern) for pattern in patterns)
                                for value in values)


def prerelease_order(label):
    return tuple((0, int(part)) if part.isdigit() else (1, part)
                 for part in label[1:].split("."))


def parse_version(text):
    if not isinstance(text, str) or len(text) > 128:
        return None
    match = re.fullmatch(r"v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-[0-9A-Za-z.-]+)?"
                         r"(?:\+[0-9A-Za-z.-]+)?", text)
    return (*map(int, match.groups()[:3]), match[4]) if match else None


def version_increases(old, new):
    if old[:3] != new[:3]:
        return old[:3] < new[:3]
    if new[3] is None:
        return old[3] is not None
    return old[3] is not None and prerelease_order(old[3]) < prerelease_order(new[3])


def bump_shape(before, after):
    old, new = parse_version(before), parse_version(after)
    if old is None or new is None or not version_increases(old, new):
        return None
    if old[3] or new[3] or old[0] != new[0]:
        return "major"
    if old[1] != new[1]:
        return "major" if old[0] == 0 else "minor"
    if old[0] == 0:
        return "major" if old[1] == 0 else "minor"
    return "patch"


def has_pattern_list(value):
    return isinstance(value, list) and bool(value) and all(
        isinstance(pattern, str) and bool(pattern.strip()) for pattern in value
    )


def valid_rule(tier, rule):
    if not isinstance(rule, dict) or rule.keys() - RULE_FIELDS:
        return False
    if any(key in rule and not has_pattern_list(rule[key]) for key in LIST_FIELDS):
        return False
    if "dependency_type" in rule and rule["dependency_type"] not in ("development", "production"):
        return False
    return tier == "C" or bool(rule.get("groups") or rule.get("ungrouped") is True or rule.get("packages"))


def rule_matches(tier, rule, pr, bumps):
    if not valid_rule(tier, rule):
        return False
    members = pr["members"]
    group = pr.get("group")
    return (
        ("groups" not in rule or group in rule["groups"])
        and ("ungrouped" not in rule or
             rule["ungrouped"] is True and not group and len(members) == 1)
        and ("packages" not in rule or matches([member["package"] for member in members], rule["packages"]))
        and ("ecosystems" not in rule or pr.get("ecosystem") in rule["ecosystems"])
        and ("directories" not in rule or matches(pr.get("directories", []), rule["directories"]))
        and ("dependency_type" not in rule or
             all(member.get("dependency_type") == rule["dependency_type"] for member in members))
        and (tier == "C" or (
            rule.get("max_bump", "minor") in BUMP_RANK
            and all(bump is not None and BUMP_RANK[bump] <= BUMP_RANK[rule.get("max_bump", "minor")]
                    for bump in bumps)
        ))
    )


def bundled_change(policy, pr):
    if pr.get("ecosystem") == "docker" or pr.get("bundled_engine") is True:
        return True
    patterns = policy.get("bundled_engines", [])
    if any(matches([member["package"]], patterns) for member in pr["members"]):
        return True
    group = pr.get("group")
    return bool(group and matches([group], policy.get("bundled_engine_groups", [])))


def critical_surface(member, configured):
    return (member.get("surface") == "critical"
            or member.get("security_sensitive") is True
            or matches([member["package"]], configured))


def deep_review_reason(policy, pr, assessed, explicit_c):
    if bundled_change(policy, pr):
        return "Bundled engine or image changed"
    if pr.get("surprise_diff") is True:
        return "Unexpected dependency diff footprint"
    if (pr.get("confirmed_risk") is True
            or any(member.get("release") == "breaking" for member, _, _ in assessed)):
        return "Confirmed breaking change or dependency risk"
    if explicit_c:
        return "Matched an explicit Tier C rule"
    if any(core and (bump == "major" or member.get("release") != "clear")
           for member, bump, core in assessed):
        return "Critical-surface major or unresolved release risk"
    return None


def complete_evidence(pr, assessed):
    if not assessed or pr.get("footprint_verified") is not True:
        return False
    flags = ("bundled_engine", "surprise_diff", "confirmed_risk")
    if any(pr.get(flag) is not False for flag in flags):
        return False
    return all(bump is not None and member.get("release") == "clear"
               and member.get("surface") in ("routine", "critical")
               and type(member.get("security_sensitive")) is bool
               for member, bump, _ in assessed)


def advisory_tier(advisory, assessed):
    severity = advisory.get("severity")
    refs = ("advisory_id", "advisory_source", "live_exposure_receipt")
    if not all(isinstance(advisory.get(ref), str) and advisory[ref].strip() for ref in refs):
        return None
    if (any(bump != "patch" for _, bump, _ in assessed)
            or advisory.get("applies_to_exact_versions") is not True
            or advisory.get("exposed_in_live_production") is not True):
        return None
    return {"medium": "B", "high": "A", "critical": "A"}.get(severity)


def risk_decision(policy, pr, assessed, explicit_c):
    if policy.get("risk_policy") != "evidence-v1":
        return "UNKNOWN", "Evidence-based policy is not enabled", False
    risk = deep_review_reason(policy, pr, assessed, explicit_c)
    if risk:
        return "C", risk, False
    if not complete_evidence(pr, assessed):
        return "UNKNOWN", "Tier evidence is incomplete", False
    advisory = pr.get("advisory")
    if advisory:
        tier = advisory_tier(advisory, assessed)
        if not tier:
            return "UNKNOWN", "Security patch needs verified version and live exposure evidence", False
        return tier, f"Verified live {advisory['severity']} security patch", True
    if any(bump == ("minor" if core else "major") for _, bump, core in assessed):
        return "B", "Critical minor or routine major with clear release evidence", False
    return "A", "Routine patch/minor or critical patch with clear release evidence", False


def classify(config, pr):
    """Use maintainer-verified facts. CI, approval, base and conflict are separate."""
    policy = config.get("weekly_deps", {})
    configured = policy.get("critical_packages", [])
    extra = policy.get("security_sensitive_extra", [])
    members = [
        {**member, "security_sensitive": True}
        if matches([member["package"]], extra) else member
        for member in pr["members"]
    ]
    assessed = [(member, bump_shape(member.get("old"), member.get("new")),
                 critical_surface(member, configured)) for member in members]
    bumps = [bump for _, bump, _ in assessed]
    rules = policy.get("tiers", {})
    explicit_c = any(rule_matches("C", rule, pr, bumps) for rule in rules.get("C", {}).values())
    tier, reason, emergency = risk_decision(policy, pr, assessed, explicit_c)
    matching = [name for name, rule in rules.get(tier, {}).items()
                if rule_matches(tier, rule, pr, bumps)]
    sensitive = any(member.get("security_sensitive") is True for member, _, _ in assessed)
    return {"tier": tier, "reason": reason, "matching_rules": matching,
            "eligible_policy": tier in ("A", "B") and (emergency or bool(matching)),
            "security_review_required": sensitive or emergency}


def main():
    argparse.ArgumentParser(description=__doc__).parse_args()
    evidence = json.load(sys.stdin)
    config, records = evidence["config"], evidence["records"]
    print(json.dumps({key: classify(config, pr) for key, pr in records.items()}, indent=2))


if __name__ == "__main__":
    main()
