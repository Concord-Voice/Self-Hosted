# Sonar CI Pip dependency analysis warnings — Plan

1. Generate a CycloneDX SBOM of the exact Linux CI pins from the three manifests without installing packages. Fail on unsupported input or markers.
2. Configure Sonar to import the SBOM, exclude exactly the three Pip manifests from ordinary resolution, and remove `sonar.sca.pythonNoResolve=true`. Run generation only on the main-branch scan; keep protected PR SCA disabled.
3. Run the focused generator and workflow tests. Follow CI and review gates. After an approved merge, run the main-branch SonarCloud analysis and verify the warnings are gone and the pinned packages are listed.
