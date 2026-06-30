CREATE TABLE shares (
    share_name TEXT NOT NULL UNIQUE,
    share_type TEXT NOT NULL,
    server_name TEXT NOT NULL,
    api_password TEXT NOT NULL DEFAULT '',
    bucket TEXT NOT NULL DEFAULT '',
    remark TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    data_shards INT NOT NULL DEFAULT 0,
    parity_shards INT NOT NULL DEFAULT 0
);

CREATE TABLE workgroups (
    id SERIAL PRIMARY KEY,
    uuid BYTEA NOT NULL,
    name TEXT UNIQUE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT workgroups_uuid_length CHECK (octet_length(uuid) = 16),
    CONSTRAINT workgroups_unique UNIQUE (uuid)
);

CREATE TABLE accounts (
    id SERIAL PRIMARY KEY,
    account_name TEXT NOT NULL,
    password_hash BYTEA NOT NULL,
    workgroup INT NOT NULL REFERENCES workgroups(id) ON DELETE CASCADE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT accounts_password_hash_length CHECK (octet_length(password_hash) = 16),
    CONSTRAINT accounts_unique UNIQUE (account_name, workgroup)
);

CREATE TABLE connections (
    workgroup INT NOT NULL REFERENCES workgroups(id) ON DELETE CASCADE,
    share_name TEXT NOT NULL REFERENCES shares(share_name) ON DELETE CASCADE,
    app_key BYTEA,
    CONSTRAINT connections_unique UNIQUE (workgroup, share_name),
    CONSTRAINT connections_app_key_length CHECK (app_key IS NULL OR octet_length(app_key) = 64)
);
CREATE INDEX idx_connections_workgroup ON connections (workgroup);
CREATE INDEX idx_connections_share ON connections (share_name);

CREATE TABLE policies (
    share_name TEXT NOT NULL,
    account INT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    workgroup INT NOT NULL,
    read_access BOOLEAN NOT NULL,
    write_access BOOLEAN NOT NULL,
    delete_access BOOLEAN NOT NULL,
    execute_access BOOLEAN NOT NULL,
    CONSTRAINT policies_share_fk FOREIGN KEY (share_name) REFERENCES shares(share_name) ON DELETE CASCADE,
    CONSTRAINT policies_connection_fk FOREIGN KEY (workgroup, share_name) REFERENCES connections(workgroup, share_name) ON DELETE CASCADE,
    CONSTRAINT share_account UNIQUE (share_name, account)
);
CREATE INDEX idx_policies_account ON policies (account);

CREATE TABLE bans (
    host TEXT UNIQUE NOT NULL,
    reason TEXT NOT NULL DEFAULT ''
);

CREATE TABLE directories (
    id BIGSERIAL,
    share_name TEXT NOT NULL,
    parent_id BIGINT,
    name TEXT NOT NULL,
    full_path TEXT NOT NULL,
    account INT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    workgroup INT NOT NULL REFERENCES workgroups(id) ON DELETE CASCADE,
    private BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    modified_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (share_name, id),
    UNIQUE (id),
    FOREIGN KEY (share_name) REFERENCES shares(share_name) ON DELETE CASCADE,
    FOREIGN KEY (share_name, parent_id) REFERENCES directories(share_name, id) ON DELETE CASCADE,
    UNIQUE (share_name, full_path),
    UNIQUE (share_name, parent_id, name)
);

CREATE TABLE objects (
    id BIGSERIAL PRIMARY KEY,
    share_name TEXT NOT NULL,
    directory_id BIGINT,
    name TEXT NOT NULL,
    full_path TEXT NOT NULL,
    size BIGINT NOT NULL DEFAULT 0,
    account INT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    workgroup INT NOT NULL REFERENCES workgroups(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    modified_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    temporary BOOLEAN NOT NULL DEFAULT FALSE,
    FOREIGN KEY (share_name) REFERENCES shares(share_name) ON DELETE CASCADE,
    FOREIGN KEY (share_name, directory_id) REFERENCES directories(share_name, id) ON DELETE CASCADE
);

CREATE INDEX idx_directories_lookup_path ON directories (share_name, full_path);
CREATE INDEX idx_directories_lookup_parent ON directories (parent_id);
CREATE INDEX idx_directories_list ON directories (share_name, parent_id, name);
CREATE INDEX idx_objects_list ON objects (share_name, directory_id, name);
CREATE INDEX idx_objects_lookup_directory ON objects (directory_id);
CREATE UNIQUE INDEX idx_objects_lookup_path ON objects (share_name, full_path) WHERE temporary = FALSE;
CREATE UNIQUE INDEX idx_objects_lookup_entry ON objects (share_name, directory_id, name) WHERE temporary = FALSE;

CREATE TABLE buffers (
    id BIGSERIAL PRIMARY KEY,
    share_name TEXT NOT NULL,
    data BYTEA NOT NULL,
    CONSTRAINT buffers_share_fk FOREIGN KEY (share_name) REFERENCES shares(share_name) ON DELETE CASCADE
);

CREATE TABLE uploads (
    id BIGSERIAL PRIMARY KEY,
    upload_id BYTEA NOT NULL UNIQUE,
    object_id BIGINT NOT NULL REFERENCES objects(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uploads_id_length CHECK (octet_length(upload_id) = 32)
);

CREATE TABLE metadata (
    id BIGSERIAL PRIMARY KEY,
    object_id BIGINT NOT NULL,
    obj_offset BIGINT NOT NULL,
    upload_id BIGINT REFERENCES uploads(id) ON DELETE CASCADE,
    slab_key BYTEA,
    buffer_id BIGINT,
    data_offset BIGINT NOT NULL,
    data_length BIGINT NOT NULL,
    CONSTRAINT metadata_object_fk FOREIGN KEY (object_id) REFERENCES objects(id) ON DELETE CASCADE,
    CONSTRAINT metadata_slab_key_length CHECK (slab_key IS NULL OR octet_length(slab_key) = 32),
    CONSTRAINT metadata_buffer_fk FOREIGN KEY (buffer_id) REFERENCES buffers(id),
    CONSTRAINT metadata_storage_check CHECK (
        (slab_key IS NOT NULL AND buffer_id IS NULL) OR
        (slab_key IS NULL AND buffer_id IS NOT NULL)
    )
);

CREATE INDEX idx_metadata_object ON metadata (object_id);
CREATE INDEX idx_metadata_offset ON metadata (object_id, obj_offset);
CREATE INDEX idx_metadata_slab_key ON metadata (slab_key);
CREATE INDEX idx_metadata_slab_key_offset ON metadata (slab_key, data_offset);
CREATE INDEX idx_metadata_upload_id ON metadata (upload_id);
CREATE UNIQUE INDEX idx_metadata_object_offset ON metadata (object_id, obj_offset);

CREATE TABLE upload_jobs (
    id BIGSERIAL PRIMARY KEY,
    upload_id BIGINT NOT NULL REFERENCES uploads(id) ON DELETE CASCADE,
    metadata_id BIGINT NOT NULL UNIQUE REFERENCES metadata(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);