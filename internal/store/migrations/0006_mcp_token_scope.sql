-- MCP tokens gain a scope column so an MCP credential can be limited to reading
-- its own account (the historical behaviour) or explicitly allow driving the
-- management API from an agent. Existing tokens default to the read-only scope:
-- a schema upgrade must never hand an already-issued credential more power.
ALTER TABLE mcp_tokens ADD COLUMN scope TEXT NOT NULL DEFAULT 'query';
