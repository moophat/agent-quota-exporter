-- DDL of the opencode `credential` table, copied from the .schema of a real opencode.db (1.18.33);
-- rows are invented, keys are canaries.
CREATE TABLE `credential` (
          `id` text PRIMARY KEY,
          `integration_id` text,
          `label` text NOT NULL,
          `value` text NOT NULL,
          `connector_id` text,
          `method_id` text,
          `active` integer,
          `time_created` integer NOT NULL,
          `time_updated` integer NOT NULL
        );
INSERT INTO credential (id, integration_id, label, value, active, time_created, time_updated)
  VALUES ('cred_000000000000000000000001', 'other-provider', 'other', '{"type":"oauth","access":"CANARY-OAUTH-ACCESS"}', 1, 1767225600000, 1767225600000);
