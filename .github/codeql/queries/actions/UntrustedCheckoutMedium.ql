/**
 * @name Checkout of untrusted code in a non-privileged context
 * @description Checking out and running the build script from a fork executes untrusted code. Even in a
 *              non-privileged workflow, this can be abused, for example to compromise self-hosted runners
 *              or to poison caches and artifacts that are later consumed by privileged workflows.
 * @kind problem
 * @problem.severity warning
 * @precision medium
 * @security-severity 5.0
 * @id concord/actions-untrusted-checkout/medium
 * @tags actions
 *       security
 *       external/cwe/cwe-829
 */

// Based on github/codeql@9b72529516caf831cfce2a1e2562e9ed717ab067;
// the final guard is scoped to Self-Hosted's protected IOC workflow checkout.

import actions
import codeql.actions.security.UntrustedCheckoutQuery

from PRHeadCheckoutStep checkout
where
  // the checkout occurs in a non-privileged context
  inNonPrivilegedContext(checkout) and
  // The required workflow's immutable job.workflow_sha is not PR-controlled.
  not (
    checkout.getLocation().getFile().getRelativePath() = ".github/workflows/ioc-scanning.yml" and
    getCheckoutReferenceText(getCheckoutReference(checkout)) = "fromJSON(toJSON(job)).workflow_sha"
  )
select checkout, "Potential unsafe checkout of untrusted pull request on non-privileged workflow."
