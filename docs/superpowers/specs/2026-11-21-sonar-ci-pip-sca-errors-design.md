# Sonar CI Pip dependency analysis warnings — Design

Issueless ad-hoc lifecycle, 2026-11-21.

SonarCloud reports disabled Pip resolution for three `.github` CI tooling manifests because `sonar.sca.pythonNoResolve=true` is global. The Semgrep manifest deliberately overrides upstream dependency constraints and installs with `--no-deps`, so enabling ordinary Pip resolution for it would fail. These manifests are CI inputs tracked by Dependabot.

Exclude those three paths from application SCA and remove the global Python resolution opt-out. Keep SCA disabled in the protected PR scan, which receives PR-controlled content and holds `SONAR_TOKEN`.

Acceptance: the next main-branch SonarCloud analysis has no Pip resolution warnings for these three paths; application dependency SCA and the protected PR security boundary remain intact.
