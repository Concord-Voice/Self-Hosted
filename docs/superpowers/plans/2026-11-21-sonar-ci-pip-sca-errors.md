# Sonar CI Pip dependency analysis warnings — Plan

1. Update `sonar-project.properties` to exclude exactly the three CI Pip manifests from SCA and remove `sonar.sca.pythonNoResolve=true`.
2. Check the property syntax and the protected PR scanner's `sonar.sca.enabled=false` override. Run the focused Sonar workflow tests.
3. Open the PR and follow CI and review gates. After an approved merge, run the main-branch SonarCloud analysis and verify the three warnings are gone.
