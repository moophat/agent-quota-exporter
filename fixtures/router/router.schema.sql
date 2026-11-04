-- 9router data.sqlite: the tables a read-only plugin may touch. Observed 2026-11-04.
-- providerConnections.data is JSON with secrets (accessToken, refreshToken, idToken, apiKey) next to health fields
-- (testStatus, errorCode, rateLimitedUntil, lastErrorAt): json_extract only the health fields, never select data.
-- usageHistory.apiKey is secret: never select it.
CREATE TABLE providerConnections (id TEXT, provider TEXT, authType TEXT, name TEXT, email TEXT, priority INTEGER,
  isActive INTEGER, data TEXT, createdAt TEXT, updatedAt TEXT);
CREATE TABLE usageHistory (id INTEGER PRIMARY KEY, timestamp TEXT, provider TEXT, model TEXT, connectionId TEXT, apiKey TEXT,
  endpoint TEXT, promptTokens INTEGER, completionTokens INTEGER, cost REAL, status TEXT, tokens TEXT, meta TEXT);
