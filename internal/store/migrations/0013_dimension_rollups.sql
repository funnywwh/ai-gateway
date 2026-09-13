-- Derived request statistics. No historical scan runs inside this migration.
CREATE TABLE request_dimension_hours (
    hour INTEGER PRIMARY KEY,
    version INTEGER NOT NULL DEFAULT 1,
    published_generation INTEGER,
    published_version INTEGER
);
CREATE TABLE request_dimension_generations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    hour INTEGER NOT NULL,
    source_version INTEGER NOT NULL
);
CREATE TABLE request_dimension_rollups (
    id INTEGER PRIMARY KEY,
    generation INTEGER NOT NULL,
    hour INTEGER NOT NULL,
    account_id INTEGER NOT NULL,
    api_key_id INTEGER NOT NULL,
    client TEXT NOT NULL,
    model TEXT NOT NULL,
    resolved_model TEXT NOT NULL,
    workspace TEXT NOT NULL,
    session_id TEXT NOT NULL,
    call_kind TEXT NOT NULL,
    title TEXT NOT NULL,
    requests INTEGER NOT NULL,
    metered INTEGER NOT NULL,
    first_seen INTEGER NOT NULL,
    last_seen INTEGER NOT NULL,
    input_tokens INTEGER NOT NULL,
    cached_tokens INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL,
    reasoning_tokens INTEGER NOT NULL,
    cost_micros INTEGER NOT NULL,
    charge_micros INTEGER NOT NULL
);
CREATE INDEX idx_dimension_rollups_generation ON request_dimension_rollups(generation);
CREATE TABLE request_dimension_progress (
    id INTEGER PRIMARY KEY CHECK(id=1),
    cursor INTEGER NOT NULL DEFAULT 0,
    complete INTEGER NOT NULL DEFAULT 0
);
INSERT INTO request_dimension_progress(id) VALUES(1);

CREATE TRIGGER dimension_log_insert AFTER INSERT ON request_logs
BEGIN
  INSERT INTO request_dimension_hours(hour) VALUES((NEW.created_at - ((NEW.created_at % 3600 + 3600) % 3600)))
  ON CONFLICT(hour) DO UPDATE SET version=version+1;
END;

CREATE TRIGGER dimension_log_delete AFTER DELETE ON request_logs
BEGIN
  INSERT INTO request_dimension_hours(hour) VALUES((OLD.created_at - ((OLD.created_at % 3600 + 3600) % 3600)))
  ON CONFLICT(hour) DO UPDATE SET version=version+1;
END;

CREATE TRIGGER dimension_log_update AFTER UPDATE OF created_at, request_id, account_id, api_key_id, client, model, resolved_model, workspace, session_id, call_kind, title ON request_logs
WHEN OLD.created_at IS NOT NEW.created_at OR OLD.request_id IS NOT NEW.request_id OR OLD.account_id IS NOT NEW.account_id OR OLD.api_key_id IS NOT NEW.api_key_id OR OLD.client IS NOT NEW.client OR OLD.model IS NOT NEW.model OR OLD.resolved_model IS NOT NEW.resolved_model OR OLD.workspace IS NOT NEW.workspace OR OLD.session_id IS NOT NEW.session_id OR OLD.call_kind IS NOT NEW.call_kind OR OLD.title IS NOT NEW.title
BEGIN
  INSERT INTO request_dimension_hours(hour) VALUES((OLD.created_at - ((OLD.created_at % 3600 + 3600) % 3600)))
  ON CONFLICT(hour) DO UPDATE SET version=version+1;
  INSERT INTO request_dimension_hours(hour) VALUES((NEW.created_at - ((NEW.created_at % 3600 + 3600) % 3600)))
  ON CONFLICT(hour) DO UPDATE SET version=version+1;
END;

CREATE TRIGGER dimension_usage_insert AFTER INSERT ON usage_records
BEGIN
  INSERT INTO request_dimension_hours(hour)
  SELECT (r.created_at - ((r.created_at % 3600 + 3600) % 3600)) FROM request_logs r WHERE r.request_id=NEW.request_id
  ON CONFLICT(hour) DO UPDATE SET version=version+1;
END;

CREATE TRIGGER dimension_usage_delete AFTER DELETE ON usage_records
BEGIN
  INSERT INTO request_dimension_hours(hour)
  SELECT (r.created_at - ((r.created_at % 3600 + 3600) % 3600)) FROM request_logs r WHERE r.request_id=OLD.request_id
  ON CONFLICT(hour) DO UPDATE SET version=version+1;
END;

CREATE TRIGGER dimension_usage_update AFTER UPDATE OF request_id, dimensions_json, cost_micros, charge_micros ON usage_records
WHEN OLD.request_id IS NOT NEW.request_id OR OLD.dimensions_json IS NOT NEW.dimensions_json OR OLD.cost_micros IS NOT NEW.cost_micros OR OLD.charge_micros IS NOT NEW.charge_micros
BEGIN
  INSERT INTO request_dimension_hours(hour)
  SELECT (r.created_at - ((r.created_at % 3600 + 3600) % 3600)) FROM request_logs r WHERE r.request_id=OLD.request_id
  ON CONFLICT(hour) DO UPDATE SET version=version+1;
  INSERT INTO request_dimension_hours(hour)
  SELECT (r.created_at - ((r.created_at % 3600 + 3600) % 3600)) FROM request_logs r WHERE r.request_id=NEW.request_id
  ON CONFLICT(hour) DO UPDATE SET version=version+1;
END;

