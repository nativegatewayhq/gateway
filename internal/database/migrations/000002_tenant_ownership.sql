CREATE TABLE organizations (
    id text PRIMARY KEY CHECK (id ~ '^org_[a-zA-Z0-9_-]{1,120}$'),
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    slug text NOT NULL CHECK (slug = lower(slug) AND slug ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (slug)
);

CREATE TABLE users (
    id text PRIMARY KEY CHECK (id ~ '^usr_[a-zA-Z0-9_-]{1,120}$'),
    external_subject text NOT NULL UNIQUE CHECK (length(external_subject) BETWEEN 1 AND 500),
    email text NOT NULL CHECK (length(email) BETWEEN 3 AND 320 AND email = lower(email)),
    display_name text NOT NULL CHECK (length(display_name) BETWEEN 1 AND 200),
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_email_lower_unique ON users (lower(email));

CREATE TABLE organization_memberships (
    organization_id text NOT NULL REFERENCES organizations(id),
    user_id text NOT NULL REFERENCES users(id),
    role text NOT NULL CHECK (role IN ('owner', 'admin', 'member')),
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (organization_id, user_id)
);

CREATE TABLE projects (
    id text PRIMARY KEY CHECK (id ~ '^project_[a-zA-Z0-9_-]{1,120}$'),
    organization_id text NOT NULL REFERENCES organizations(id),
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    slug text NOT NULL CHECK (slug = lower(slug) AND slug ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    environment text NOT NULL DEFAULT 'development' CHECK (environment IN ('development', 'production')),
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organization_id, slug), UNIQUE (id, organization_id)
);

INSERT INTO organizations (id, name, slug) VALUES ('org_legacy', 'Legacy self-hosted organization', 'legacy') ON CONFLICT (id) DO NOTHING;
INSERT INTO projects (id, organization_id, name, slug, environment) VALUES ('project_legacy', 'org_legacy', 'Legacy development project', 'legacy', 'development') ON CONFLICT (id) DO NOTHING;

ALTER TABLE service_api_keys ADD COLUMN project_id text;
UPDATE service_api_keys SET project_id = 'project_legacy' WHERE project_id IS NULL;
ALTER TABLE service_api_keys ALTER COLUMN project_id SET NOT NULL;
ALTER TABLE service_api_keys ADD CONSTRAINT service_api_keys_project_fk FOREIGN KEY (project_id) REFERENCES projects(id);
CREATE INDEX service_api_keys_project_idx ON service_api_keys(project_id);
