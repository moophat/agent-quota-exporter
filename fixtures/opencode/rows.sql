-- Synthetic rows matching opencode.schema.sql (project, session, 3 assistant/user messages). All values are invented.
-- message.data is JSON; the exporter reads it only through json_extract of the allow-listed paths.
INSERT INTO project (id, worktree, vcs, name, time_created, time_updated, sandboxes)
VALUES ('prj_00000000000000000001', '/root/example-project', 'git', 'example-project', 1767225600000, 1767225600000, '[]');

INSERT INTO session (id, project_id, parent_id, slug, directory, title, version, cost, tokens_input, tokens_output,
                     tokens_reasoning, tokens_cache_read, tokens_cache_write, agent, model, time_created, time_updated)
VALUES ('ses_00000000000000000001', 'prj_00000000000000000001', NULL, 'example-slug', '/root/example-project',
        'example title', '0.0.0', 0.0111, 24545, 3, 16, 100, 0, 'build', 'provider-a/model-y', 1767225600000, 1767225700000);

INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES
('msg_00000000000000000001', 'ses_00000000000000000001', 1767225601000, 1767225601000,
 '{"role":"user","time":{"created":1767225601000},"agent":"build","model":{"providerID":"provider-a","modelID":"model-y"}}'),
('msg_00000000000000000002', 'ses_00000000000000000001', 1767225605000, 1767225609000,
 '{"role":"assistant","time":{"created":1767225605000,"completed":1767225609000},"parentID":"msg_00000000000000000001","modelID":"model-y","providerID":"provider-a","mode":"build","agent":"build","cost":0.0037,"tokens":{"total":24564,"input":24545,"output":3,"reasoning":16,"cache":{"write":0,"read":0}},"finish":"stop"}'),
('msg_00000000000000000003', 'ses_00000000000000000001', 1767225650000, 1767225651000,
 '{"role":"assistant","time":{"created":1767225650000,"completed":1767225651000},"parentID":"msg_00000000000000000001","modelID":"model-y","providerID":"provider-a","mode":"build","agent":"build","cost":0,"tokens":{"total":0,"input":0,"output":0,"reasoning":0,"cache":{"write":0,"read":0}},"error":{"name":"APIError","data":{"statusCode":429}}}');
