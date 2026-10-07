-- AI Gateway database schema, PostgreSQL 14+
--
-- This migration only creates the schema. It does not create an administrator,
-- provider keys, or model rows. Those are bootstrap/application concerns.

BEGIN;

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE SCHEMA IF NOT EXISTS ai_gateway;

CREATE TABLE IF NOT EXISTS ai_gateway.schema_migrations (
    version     text PRIMARY KEY,
    applied_at  timestamptz NOT NULL DEFAULT now()
);

-- Human users of the gateway. shared_model_key_id is the requested shortcut:
-- a user may use that model without a separate model permission grant.
CREATE TABLE IF NOT EXISTS ai_gateway.users (
    id                   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    username             text NOT NULL UNIQUE,
    display_name         text,
    password_hash        text,
    role                 text NOT NULL DEFAULT 'user'
        CHECK (role IN ('admin', 'operator', 'user')),
    status               text NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'disabled')),
    shared_model_key_id  bigint,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now()
);

-- A logical/public model. A model key can be associated with many models via
-- model_key_models below.
CREATE TABLE IF NOT EXISTS ai_gateway.models (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name            text NOT NULL UNIQUE,
    provider        text NOT NULL,
    upstream_model  text NOT NULL,
    enabled         boolean NOT NULL DEFAULT true,
    metadata        jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- Upstream provider/account keys. Store encrypted ciphertext here; never store
-- the provider secret in plaintext. api_key_fingerprint is safe to display and
-- is used to prevent accidental duplicate imports.
CREATE TABLE IF NOT EXISTS ai_gateway.model_keys (
    id                    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- Kept as a compatibility pointer for old rows. New associations live in
    -- model_key_models, allowing one account key to serve several models.
    model_id              bigint REFERENCES ai_gateway.models(id) ON DELETE SET NULL,
    account_name          text NOT NULL,
    auth_type             text NOT NULL DEFAULT 'api_key'
        CHECK (auth_type IN ('api_key', 'codex_oauth')),
    api_key_ciphertext    text NOT NULL,
    api_key_fingerprint   text NOT NULL UNIQUE,
    status                text NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'disabled', 'exhausted')),
    priority              integer NOT NULL DEFAULT 0,
    max_concurrency       integer NOT NULL DEFAULT 1
        CHECK (max_concurrency > 0),
    created_by_user_id    bigint REFERENCES ai_gateway.users(id) ON DELETE SET NULL,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS ai_gateway.model_key_models (
    model_key_id bigint NOT NULL REFERENCES ai_gateway.model_keys(id) ON DELETE CASCADE,
    model_id     bigint NOT NULL REFERENCES ai_gateway.models(id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (model_key_id, model_id)
);

-- One current row per provider key. active_requests prevents two requests from
-- incorrectly claiming a single-account key; cooldown_until handles rate limits.
CREATE TABLE IF NOT EXISTS ai_gateway.model_key_runtime (
    model_key_id       bigint PRIMARY KEY REFERENCES ai_gateway.model_keys(id) ON DELETE CASCADE,
    state              text NOT NULL DEFAULT 'idle'
        CHECK (state IN ('idle', 'in_use', 'cooldown', 'disabled')),
    active_requests    integer NOT NULL DEFAULT 0 CHECK (active_requests >= 0),
    last_used_at       timestamptz,
    cooldown_until     timestamptz,
    last_error_code    text,
    version            bigint NOT NULL DEFAULT 0,
    updated_at         timestamptz NOT NULL DEFAULT now()
);

-- A lease per request. Expired leases can be reclaimed after a process crash.
CREATE TABLE IF NOT EXISTS ai_gateway.model_key_leases (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    model_key_id   bigint NOT NULL REFERENCES ai_gateway.model_keys(id) ON DELETE CASCADE,
    request_id     text NOT NULL,
    user_id        bigint REFERENCES ai_gateway.users(id) ON DELETE SET NULL,
    status         text NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'released', 'expired')),
    started_at     timestamptz NOT NULL DEFAULT now(),
    expires_at     timestamptz NOT NULL,
    released_at    timestamptz
);

-- Keys presented by gateway users. Store a hash, not the raw gateway key.
CREATE TABLE IF NOT EXISTS ai_gateway.gateway_api_keys (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id        bigint NOT NULL REFERENCES ai_gateway.users(id) ON DELETE CASCADE,
    name           text NOT NULL,
    key_prefix     text NOT NULL,
    key_hash       text NOT NULL UNIQUE,
    status         text NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'revoked')),
    expires_at     timestamptz,
    last_used_at   timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    revoked_at     timestamptz
);

-- Explicit model permissions. A user's shared_model_key_id (or the join table
-- below) is checked before this table, so the shared model needs no grant.
CREATE TABLE IF NOT EXISTS ai_gateway.user_model_permissions (
    user_id          bigint NOT NULL REFERENCES ai_gateway.users(id) ON DELETE CASCADE,
    model_id         bigint NOT NULL REFERENCES ai_gateway.models(id) ON DELETE CASCADE,
    effect           text NOT NULL DEFAULT 'allow'
        CHECK (effect IN ('allow', 'deny')),
    granted_by_user_id bigint REFERENCES ai_gateway.users(id) ON DELETE SET NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, model_id)
);

-- Supports more than one shared model per user in the future. The field on
-- users remains the default/compatibility shortcut requested by the design.
CREATE TABLE IF NOT EXISTS ai_gateway.user_shared_model_keys (
    user_id       bigint NOT NULL REFERENCES ai_gateway.users(id) ON DELETE CASCADE,
    model_key_id  bigint NOT NULL REFERENCES ai_gateway.model_keys(id) ON DELETE CASCADE,
    is_default    boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, model_key_id)
);

CREATE UNIQUE INDEX IF NOT EXISTS ux_user_shared_model_default
    ON ai_gateway.user_shared_model_keys (user_id)
    WHERE is_default;

-- Request accounting and audit data.
CREATE TABLE IF NOT EXISTS ai_gateway.usage_records (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    request_id     text NOT NULL,
    user_id        bigint REFERENCES ai_gateway.users(id) ON DELETE SET NULL,
    model_id       bigint REFERENCES ai_gateway.models(id) ON DELETE SET NULL,
    model_key_id   bigint REFERENCES ai_gateway.model_keys(id) ON DELETE SET NULL,
    status         text NOT NULL CHECK (status IN ('success', 'error')),
    input_tokens   bigint NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens  bigint NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    latency_ms     bigint CHECK (latency_ms IS NULL OR latency_ms >= 0),
    error_code     text,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS ai_gateway.audit_logs (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    actor_user_id  bigint REFERENCES ai_gateway.users(id) ON DELETE SET NULL,
    action         text NOT NULL,
    resource_type  text,
    resource_id    text,
    details        jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- Add the requested users -> model_keys shortcut after both tables exist.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'fk_users_shared_model_key'
          AND conrelid = 'ai_gateway.users'::regclass
    ) THEN
        ALTER TABLE ai_gateway.users
            ADD CONSTRAINT fk_users_shared_model_key
            FOREIGN KEY (shared_model_key_id)
            REFERENCES ai_gateway.model_keys(id)
            ON DELETE SET NULL;
    END IF;
END
$$;

-- Login support added after the first rollout. Existing users remain valid
-- for API records but need a password set before they can log in.
ALTER TABLE ai_gateway.users
    ADD COLUMN IF NOT EXISTS password_hash text;

ALTER TABLE ai_gateway.model_keys
    ADD COLUMN IF NOT EXISTS auth_type text NOT NULL DEFAULT 'api_key';

-- Existing installations used model_keys.model_id as a one-to-one link.
-- Preserve those links in the new join table, then make the compatibility
-- column nullable so OAuth/API keys can be linked to several models.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'model_keys_model_id_fkey'
          AND conrelid = 'ai_gateway.model_keys'::regclass
    ) THEN
        ALTER TABLE ai_gateway.model_keys DROP CONSTRAINT model_keys_model_id_fkey;
    END IF;
END
$$;

ALTER TABLE ai_gateway.model_keys ALTER COLUMN model_id DROP NOT NULL;
ALTER TABLE ai_gateway.model_keys
    ADD CONSTRAINT model_keys_model_id_fkey
    FOREIGN KEY (model_id) REFERENCES ai_gateway.models(id) ON DELETE SET NULL;

INSERT INTO ai_gateway.model_key_models (model_key_id, model_id)
SELECT id, model_id
FROM ai_gateway.model_keys
WHERE model_id IS NOT NULL
ON CONFLICT (model_key_id, model_id) DO NOTHING;

CREATE INDEX IF NOT EXISTS ix_model_keys_selection
    ON ai_gateway.model_keys (model_id, status, priority DESC);

CREATE INDEX IF NOT EXISTS ix_model_key_models_model
    ON ai_gateway.model_key_models (model_id, model_key_id);

CREATE INDEX IF NOT EXISTS ix_model_key_runtime_selection
    ON ai_gateway.model_key_runtime (state, cooldown_until, active_requests);

CREATE INDEX IF NOT EXISTS ix_model_key_leases_expiry
    ON ai_gateway.model_key_leases (status, expires_at);

CREATE INDEX IF NOT EXISTS ix_gateway_api_keys_user_status
    ON ai_gateway.gateway_api_keys (user_id, status);

CREATE INDEX IF NOT EXISTS ix_usage_records_user_created
    ON ai_gateway.usage_records (user_id, created_at DESC);

CREATE INDEX IF NOT EXISTS ix_usage_records_model_created
    ON ai_gateway.usage_records (model_id, created_at DESC);

CREATE OR REPLACE FUNCTION ai_gateway.set_updated_at()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS trg_users_updated_at ON ai_gateway.users;
CREATE TRIGGER trg_users_updated_at
BEFORE UPDATE ON ai_gateway.users
FOR EACH ROW EXECUTE FUNCTION ai_gateway.set_updated_at();

DROP TRIGGER IF EXISTS trg_models_updated_at ON ai_gateway.models;
CREATE TRIGGER trg_models_updated_at
BEFORE UPDATE ON ai_gateway.models
FOR EACH ROW EXECUTE FUNCTION ai_gateway.set_updated_at();

DROP TRIGGER IF EXISTS trg_model_keys_updated_at ON ai_gateway.model_keys;
CREATE TRIGGER trg_model_keys_updated_at
BEFORE UPDATE ON ai_gateway.model_keys
FOR EACH ROW EXECUTE FUNCTION ai_gateway.set_updated_at();

DROP TRIGGER IF EXISTS trg_model_key_runtime_updated_at ON ai_gateway.model_key_runtime;
CREATE TRIGGER trg_model_key_runtime_updated_at
BEFORE UPDATE ON ai_gateway.model_key_runtime
FOR EACH ROW EXECUTE FUNCTION ai_gateway.set_updated_at();

CREATE OR REPLACE FUNCTION ai_gateway.init_model_key_runtime()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    INSERT INTO ai_gateway.model_key_runtime (model_key_id)
    VALUES (NEW.id)
    ON CONFLICT (model_key_id) DO NOTHING;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS trg_init_model_key_runtime ON ai_gateway.model_keys;
CREATE TRIGGER trg_init_model_key_runtime
AFTER INSERT ON ai_gateway.model_keys
FOR EACH ROW EXECUTE FUNCTION ai_gateway.init_model_key_runtime();

INSERT INTO ai_gateway.schema_migrations (version)
VALUES ('001_initial_schema')
ON CONFLICT (version) DO NOTHING;

-- The application role is created by deployment setup rather than this
-- migration. Grant permissions when it already exists, including the join
-- table added for multi-model keys.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ai_gateway_app') THEN
        EXECUTE 'GRANT USAGE ON SCHEMA ai_gateway TO ai_gateway_app';
        EXECUTE 'GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA ai_gateway TO ai_gateway_app';
        EXECUTE 'GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA ai_gateway TO ai_gateway_app';
        EXECUTE 'ALTER DEFAULT PRIVILEGES IN SCHEMA ai_gateway GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO ai_gateway_app';
        EXECUTE 'ALTER DEFAULT PRIVILEGES IN SCHEMA ai_gateway GRANT USAGE, SELECT ON SEQUENCES TO ai_gateway_app';
    END IF;
END
$$;

COMMIT;
