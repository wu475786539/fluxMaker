CREATE TABLE IF NOT EXISTS schema_migrations (
    version TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS users (
    id BIGSERIAL PRIMARY KEY,
    email TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS users_email_lower_idx ON users (lower(email));

CREATE TABLE IF NOT EXISTS roles (
    id BIGSERIAL PRIMARY KEY,
    code TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    all_instruments BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE TABLE IF NOT EXISTS permissions (
    code TEXT PRIMARY KEY,
    name TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS user_roles (
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role_id BIGINT NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, role_id)
);

CREATE TABLE IF NOT EXISTS role_permissions (
    role_id BIGINT NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    permission_code TEXT NOT NULL REFERENCES permissions(code) ON DELETE CASCADE,
    PRIMARY KEY (role_id, permission_code)
);

CREATE TABLE IF NOT EXISTS role_instruments (
    role_id BIGINT NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    instrument_id TEXT NOT NULL,
    PRIMARY KEY (role_id, instrument_id)
);

CREATE TABLE IF NOT EXISTS draft_configs (
    id SMALLINT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    payload JSONB NOT NULL,
    updated_by BIGINT REFERENCES users(id),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS config_snapshots (
    id BIGSERIAL PRIMARY KEY,
    version BIGINT NOT NULL UNIQUE,
    payload JSONB NOT NULL,
    active BOOLEAN NOT NULL DEFAULT FALSE,
    published_by BIGINT REFERENCES users(id),
    published_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS config_snapshots_one_active_idx ON config_snapshots (active) WHERE active;

CREATE TABLE IF NOT EXISTS audit_logs (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT REFERENCES users(id),
    action TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id TEXT,
    details JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS audit_logs_created_at_idx ON audit_logs (created_at DESC);

INSERT INTO roles (code, name, all_instruments) VALUES
    ('super_admin', '超级管理员', TRUE),
    ('risk_admin', '风控管理员', TRUE),
    ('operator', '交易操作员', FALSE),
    ('viewer', '只读用户', FALSE),
    ('auditor', '审计员', TRUE)
ON CONFLICT (code) DO NOTHING;

INSERT INTO permissions (code, name) VALUES
    ('dashboard:view', '查看仪表盘'),
    ('config:view', '查看配置'),
    ('config:edit', '编辑配置草稿'),
    ('config:publish', '发布配置'),
    ('token:view', '查看 Token'),
    ('token:edit', '编辑 Token'),
    ('instrument:view', '查看币对'),
    ('instrument:edit', '编辑币对'),
    ('venue:view', '查看交易所'),
    ('venue:edit', '编辑交易所'),
    ('strategy:view', '查看策略'),
    ('strategy:edit', '编辑策略'),
    ('runtime:view', '查看运行状态'),
    ('runtime:start', '启动币对'),
    ('runtime:stop', '停止币对'),
    ('runtime:emergency_cancel', '紧急撤单'),
    ('orders:view', '查看订单'),
    ('fills:view', '查看成交'),
    ('audit:view', '查看审计'),
    ('users:manage', '管理用户'),
    ('roles:manage', '管理角色')
ON CONFLICT (code) DO NOTHING;

INSERT INTO role_permissions (role_id, permission_code)
SELECT r.id, p.code FROM roles r CROSS JOIN permissions p WHERE r.code = 'super_admin'
ON CONFLICT DO NOTHING;

INSERT INTO role_permissions (role_id, permission_code)
SELECT r.id, p.code FROM roles r JOIN permissions p ON p.code IN
    ('dashboard:view','config:view','token:view','instrument:view','venue:view','strategy:view','runtime:view','runtime:stop','runtime:emergency_cancel','orders:view','fills:view','audit:view')
WHERE r.code = 'risk_admin' ON CONFLICT DO NOTHING;

INSERT INTO role_permissions (role_id, permission_code)
SELECT r.id, p.code FROM roles r JOIN permissions p ON p.code IN
    ('dashboard:view','config:view','instrument:view','venue:view','strategy:view','runtime:view','runtime:start','runtime:stop','orders:view','fills:view')
WHERE r.code = 'operator' ON CONFLICT DO NOTHING;

INSERT INTO role_permissions (role_id, permission_code)
SELECT r.id, p.code FROM roles r JOIN permissions p ON p.code IN
    ('dashboard:view','config:view','token:view','instrument:view','venue:view','strategy:view','runtime:view','orders:view','fills:view')
WHERE r.code = 'viewer' ON CONFLICT DO NOTHING;

INSERT INTO role_permissions (role_id, permission_code)
SELECT r.id, p.code FROM roles r JOIN permissions p ON p.code IN ('config:view','audit:view','orders:view','fills:view')
WHERE r.code = 'auditor' ON CONFLICT DO NOTHING;

