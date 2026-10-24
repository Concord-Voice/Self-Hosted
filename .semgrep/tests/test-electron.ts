// Test fixture for concord-electron rules
// This file should trigger Semgrep findings — it is NOT production code.

// ruleid: concord-electron-node-integration
const win = new BrowserWindow({
  webPreferences: {
    nodeIntegration: true,
  }
});

// ruleid: concord-electron-context-isolation
const win2 = new BrowserWindow({
  webPreferences: {
    contextIsolation: false,
  }
});

// ruleid: concord-electron-sandbox
const win3 = new BrowserWindow({
  webPreferences: {
    sandbox: false,
  }
});
