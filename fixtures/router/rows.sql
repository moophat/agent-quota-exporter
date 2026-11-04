-- Synthetic rows for router.schema.sql (invented 2026-11-04, claude-opus-5-5). Every value is fake. CANARY-ROUTER-*
-- strings sit in every secret field (usageHistory.apiKey, tokens, meta; providerConnections.data accessToken,
-- refreshToken, idToken, apiKey; a non-JSON data value): no read of this fixture may ever return one.
-- Health as of 2026-11-04T12:00:00Z (Unix 1793793600). Value words follow 9router's own: usageHistory.status
-- defaults to "ok"; data.testStatus is one of active, ok, success, unknown, testing (not failed) or error, expired,
-- unavailable (failed); data.rateLimitedUntil is an ISO time (an epoch in ms is accepted too).
INSERT INTO providerConnections (rowid, id, provider, authType, name, email, priority, isActive, data, createdAt, updatedAt) VALUES
 (1, '00000000-0000-4000-8000-000000000001', 'claude', 'oauth', 'work-a', 'a1@example.com', 1, 1,
  '{"accessToken":"CANARY-ROUTER-ACCESS-1","refreshToken":"CANARY-ROUTER-REFRESH-1","idToken":"CANARY-ROUTER-IDTOKEN-1","testStatus":"active","errorCode":null,"rateLimitedUntil":null,"lastErrorAt":null}',
  '2026-01-01T00:00:00.000Z', '2026-01-01T00:00:00.000Z'),
 -- name is not label-safe: hash label (or the email in email mode); rate-limited until 13:00 (ISO)
 (2, '00000000-0000-4000-8000-000000000002', 'codex', 'oauth', 'Shared Account (b1)', 'b1@example.com', 2, 1,
  '{"accessToken":"CANARY-ROUTER-ACCESS-2","refreshToken":"CANARY-ROUTER-REFRESH-2","testStatus":"active","errorCode":429,"rateLimitedUntil":"2026-11-04T13:00:00.000Z","lastErrorAt":"2026-11-04T11:59:00.000Z"}',
  '2026-01-01T00:00:00.000Z', '2026-01-01T00:00:00.000Z'),
 -- last test failed
 (3, '00000000-0000-4000-8000-000000000003', 'OpenRouter', 'apikey', 'or-key', NULL, 3, 1,
  '{"apiKey":"CANARY-ROUTER-APIKEY-3","testStatus":"error","errorCode":401,"rateLimitedUntil":null,"lastErrorAt":"2026-11-04T11:00:00.000Z"}',
  '2026-01-01T00:00:00.000Z', '2026-01-01T00:00:00.000Z'),
 -- disabled; its rate limit ended at 10:00
 (4, '00000000-0000-4000-8000-000000000004', 'glm', 'apikey', 'glm-1', NULL, 4, 0,
  '{"apiKey":"CANARY-ROUTER-APIKEY-4","testStatus":"active","rateLimitedUntil":"2026-11-04T10:00:00.000Z"}',
  '2026-01-01T00:00:00.000Z', '2026-01-01T00:00:00.000Z'),
 -- never tested; rate-limited until 14:00 given as epoch ms
 (5, '00000000-0000-4000-8000-000000000005', 'gemini', 'oauth', 'gem-1', 'c1@example.com', 5, 1,
  '{"accessToken":"CANARY-ROUTER-ACCESS-5","testStatus":"unknown","rateLimitedUntil":1793800800000}',
  '2026-01-01T00:00:00.000Z', '2026-01-01T00:00:00.000Z'),
 -- same name as row 1: the later connection falls back to its hash
 (6, '00000000-0000-4000-8000-000000000006', 'claude', 'oauth', 'Work-A', 'd1@example.com', 6, 1,
  '{"accessToken":"CANARY-ROUTER-ACCESS-6","testStatus":"ok"}',
  '2026-01-01T00:00:00.000Z', '2026-01-01T00:00:00.000Z'),
 -- data is not JSON: every health field reads as absent
 (7, '00000000-0000-4000-8000-000000000007', 'kiro', 'oauth', 'kiro-1', NULL, 7, 1,
  'not json CANARY-ROUTER-DATA-7',
  '2026-01-01T00:00:00.000Z', '2026-01-01T00:00:00.000Z'),
 -- a testStatus word this exporter does not know: down, and counted as a parse error
 (8, '00000000-0000-4000-8000-000000000008', 'qwen', 'oauth', 'qw-1', NULL, 8, 1,
  '{"accessToken":"CANARY-ROUTER-ACCESS-8","testStatus":"brand-new-state"}',
  '2026-01-01T00:00:00.000Z', '2026-01-01T00:00:00.000Z');

INSERT INTO usageHistory (id, timestamp, provider, model, connectionId, apiKey, endpoint, promptTokens, completionTokens, cost, status, tokens, meta) VALUES
 (1, '2026-11-04T11:00:00.000Z', 'claude', 'claude-opus-4', '00000000-0000-4000-8000-000000000001', 'CANARY-ROUTER-KEY-1', '/v1/messages', 100, 10, 0.5, 'ok',
  '{"prompt_tokens":100,"completion_tokens":10,"note":"CANARY-ROUTER-TOKENS-1"}', '{"note":"CANARY-ROUTER-META-1"}'),
 (2, '2026-11-04T11:01:00.000Z', 'claude', 'claude-opus-4', '00000000-0000-4000-8000-000000000001', 'CANARY-ROUTER-KEY-1', '/v1/messages', 200, 20, 0, 'ok',
  '{"prompt_tokens":200,"completion_tokens":20}', '{}'),
 (3, '2026-11-04T11:02:00.000Z', 'codex', 'gpt-5', '00000000-0000-4000-8000-000000000002', 'CANARY-ROUTER-KEY-2', '/v1/responses', 300, 30, 0, 'ok',
  '{"input_tokens":300,"output_tokens":30}', '{}'),
 -- model case differs, status NULL reads as ok (9router does the same)
 (4, '2026-11-04T11:03:00.000Z', 'codex', 'GPT-5', '00000000-0000-4000-8000-000000000002', 'CANARY-ROUTER-KEY-2', '/v1/responses', 400, 40, 0, NULL,
  '{"input_tokens":400,"output_tokens":40}', '{}'),
 (5, '2026-11-04T11:04:00.000Z', 'openrouter', 'deepseek/deepseek-chat', '00000000-0000-4000-8000-000000000003', 'CANARY-ROUTER-KEY-3', '/v1/chat/completions', 50, 5, 0.01, 'error',
  '{"prompt_tokens":50,"completion_tokens":5}', '{}'),
 -- provider case differs; an HTTP 2xx status reads as ok
 (6, '2026-11-04T11:05:00.000Z', 'OpenRouter', 'deepseek/deepseek-chat', '00000000-0000-4000-8000-000000000003', 'CANARY-ROUTER-KEY-3', '/v1/chat/completions', 60, 6, 0.02, '200',
  '{"prompt_tokens":60,"completion_tokens":6}', '{}'),
 -- provider not label-safe
 (7, '2026-11-04T11:06:00.000Z', 'weird provider!', 'm1', '00000000-0000-4000-8000-000000000004', 'CANARY-ROUTER-KEY-4', '/v1/chat/completions', 1, 1, 0, 'ok',
  '{}', '{}'),
 -- no connection
 (8, '2026-11-04T11:07:00.000Z', 'claude', 'claude-sonnet-4', '', 'CANARY-ROUTER-KEY-5', '/v1/messages', 10, 1, 0, 'ok',
  '{}', '{}'),
 -- a connection that no longer exists
 (9, '2026-11-04T11:08:00.000Z', 'claude', 'claude-sonnet-4', '00000000-0000-4000-8000-000000000099', 'CANARY-ROUTER-KEY-6', '/v1/messages', 20, 2, 0, 'ok',
  '{}', '{}'),
 (10, '2026-11-04T11:09:00.000Z', 'claude', 'claude-opus-4', '00000000-0000-4000-8000-000000000006', 'CANARY-ROUTER-KEY-7', '/v1/messages', 5, 5, 0.1, 'failed',
  '{}', '{}');
