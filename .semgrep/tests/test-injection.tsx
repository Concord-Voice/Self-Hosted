// Test fixture for concord-injection rules
// This file should trigger Semgrep findings — it is NOT production code.

// ruleid: concord-injection-eval
const result = eval("1 + 1");

// ruleid: concord-injection-new-function
const fn = new Function("a", "b", "return a + b");

// ruleid: concord-injection-document-write
document.write("<h1>Hello</h1>");

// ruleid: concord-injection-dangerous-html
const el = <div dangerouslySetInnerHTML={{ __html: userInput }} />;

// ruleid: concord-injection-innerhtml
element.innerHTML = userInput;

// ruleid: concord-injection-exec
const out = execSync("rm -rf /tmp/cache");
