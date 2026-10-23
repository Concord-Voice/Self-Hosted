/**
 * @name Log entries created from user input (unstructured loggers)
 * @description Building a log entry from user-controlled input lets a malicious user
 *              forge log records. This is the upstream go/log-injection query with one
 *              change: a slog attribute VALUE that follows a constant key is not a sink.
 *              Messages, keys and spread arguments stay sinks.
 * @kind path-problem
 * @problem.severity error
 * @security-severity 6.1
 * @precision medium
 * @id concord/go/log-injection-unstructured
 * @tags security
 *       external/cwe/cwe-117
 */

import go
import semmle.go.security.LogInjection

/** Packages whose record calls are rendered by a slog handler. */
private string slogPackage() {
  result =
    [
      "log/slog",
      "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
    ]
}

/**
 * Holds if `call` takes an alternating key/value list starting at syntactic
 * argument `first`. `pkg/logger.Logger` embeds `*slog.Logger`, so most of its
 * calls resolve to `log/slog` methods; `Fatal` is its own.
 */
private predicate slogKeyValueCall(DataFlow::CallNode call, int first) {
  exists(Function f, string name |
    f = call.getTarget() and f.getPackage().getPath() = slogPackage() and name = f.getName()
  |
    name = ["Debug", "Info", "Warn", "Error", "Fatal", "Group"] and first = 1
    or
    name = ["DebugContext", "InfoContext", "WarnContext", "ErrorContext"] and first = 2
    or
    name = "Log" and first = 3
    or
    name = "With" and first = 0
  )
}

/**
 * An attribute VALUE whose key, and every earlier key in the same list, is a string
 * constant — or a `slog.String`/`Any`/… constructor call with a constant key. Every
 * built-in slog handler quotes or escapes attribute values, including slog's default
 * handler, so a newline there cannot start a second record.
 * `TestHandlersEscapeControlCharsInAttrValues` locks that guarantee.
 *
 * These nodes are removed from the SINKS, not added as barriers. A barrier also cuts
 * taint for every later read of the same variable: the PR #3440 canary showed a
 * sanitized `h.log.Info("x", "v", v)` hiding a later `log.Printf(v)` in the same
 * function.
 *
 * Deliberately still sinks:
 *  - the message: slog's default handler writes it raw. `cmd/server` installs the
 *    escaping `pkg/logger` handler with `slog.SetDefault`, but other binaries and
 *    tests do not, and the query cannot see which handler is live;
 *  - keys and group names: no built-in handler removes duplicate keys, so a tainted
 *    key forges a field ("level", "msg") without any newline;
 *  - spread arguments (`args...`) and lists that mix in `slog.Attr` values, whose key
 *    positions cannot be proven constant. These can false-positive, which is the
 *    safe direction.
 * A custom `slog.Handler` that writes values raw voids the guarantee.
 */
class SlogConstantKeyedValue extends DataFlow::Node {
  SlogConstantKeyedValue() {
    exists(DataFlow::CallNode call, int first, int i |
      slogKeyValueCall(call, first) and
      i > first and
      (i - first) % 2 = 1 and
      this = call.getSyntacticArgument(i) and
      forall(int k | k in [first .. i - 1] and (k - first) % 2 = 0 |
        exists(call.getSyntacticArgument(k).asExpr().getStringValue())
      )
    )
    or
    exists(DataFlow::CallNode call |
      call.getTarget()
          .hasQualifiedName("log/slog",
            ["String", "Int", "Int64", "Uint64", "Float64", "Bool", "Time", "Duration", "Any"]) and
      exists(call.getArgument(0).asExpr().getStringValue()) and
      this = call
    )
  }
}

/** The upstream go/log-injection configuration, minus constant-keyed slog values as sinks. */
module UnstructuredLogConfig implements DataFlow::ConfigSig {
  predicate isSource(DataFlow::Node source) { source instanceof LogInjection::Source }

  predicate isSink(DataFlow::Node sink) {
    sink instanceof LogInjection::Sink and not sink instanceof SlogConstantKeyedValue
  }

  predicate isBarrier(DataFlow::Node node) { node instanceof LogInjection::Sanitizer }

  predicate observeDiffInformedIncrementalMode() { any() }
}

module UnstructuredLogFlow = TaintTracking::Global<UnstructuredLogConfig>;

import UnstructuredLogFlow::PathGraph

from UnstructuredLogFlow::PathNode source, UnstructuredLogFlow::PathNode sink
where UnstructuredLogFlow::flowPath(source, sink)
select sink.getNode(), source, sink, "This log entry depends on a $@.", source.getNode(),
  "user-provided value"
