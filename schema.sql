-- buildz hardware inventory schema
-- Hybrid: append-only raw snapshots + normalized per-collection rows + flat "current" view per host.

CREATE TABLE IF NOT EXISTS hosts (
    id          SERIAL PRIMARY KEY,
    hostname    TEXT NOT NULL UNIQUE,
    label       TEXT,                       -- friendly name from hosts.yaml (e.g. "nas01")
    first_seen  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Every successful collection appends one row here. Source of truth; never update/delete.
CREATE TABLE IF NOT EXISTS collections (
    id              BIGSERIAL PRIMARY KEY,
    host_id         INT NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    collected_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    schema_version  INT NOT NULL,
    raw             JSONB NOT NULL
);
CREATE INDEX IF NOT EXISTS collections_host_time_idx
    ON collections(host_id, collected_at DESC);

-- Normalized per-collection rows. Re-inserted on each run, tied to that run's collection_id.
CREATE TABLE IF NOT EXISTS disks (
    id            BIGSERIAL PRIMARY KEY,
    host_id       INT    NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    collection_id BIGINT NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    device        TEXT,
    model         TEXT,
    serial        TEXT,
    size_bytes    BIGINT,
    rota          BOOLEAN,
    tran          TEXT,
    smart         JSONB
);
CREATE INDEX IF NOT EXISTS disks_host_idx       ON disks(host_id);
CREATE INDEX IF NOT EXISTS disks_collection_idx ON disks(collection_id);

CREATE TABLE IF NOT EXISTS nics (
    id            BIGSERIAL PRIMARY KEY,
    host_id       INT    NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    collection_id BIGINT NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    ifname        TEXT,
    mac           TEXT,
    speed_mbps    INT,
    driver        TEXT,
    pci_id        TEXT
);
CREATE INDEX IF NOT EXISTS nics_host_idx       ON nics(host_id);
CREATE INDEX IF NOT EXISTS nics_collection_idx ON nics(collection_id);

CREATE TABLE IF NOT EXISTS memory_modules (
    id            BIGSERIAL PRIMARY KEY,
    host_id       INT    NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    collection_id BIGINT NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    locator       TEXT,
    size_bytes    BIGINT,
    speed_mts     INT,
    manufacturer  TEXT,
    part_number   TEXT,
    serial        TEXT
);
CREATE INDEX IF NOT EXISTS memory_host_idx       ON memory_modules(host_id);
CREATE INDEX IF NOT EXISTS memory_collection_idx ON memory_modules(collection_id);

CREATE TABLE IF NOT EXISTS gpus (
    id            BIGSERIAL PRIMARY KEY,
    host_id       INT    NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    collection_id BIGINT NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    vendor        TEXT,
    model         TEXT,
    pci_id        TEXT,
    vram_bytes    BIGINT
);
CREATE INDEX IF NOT EXISTS gpus_host_idx ON gpus(host_id);

-- Flat "latest snapshot" per host. One row per host, upserted on every run.
CREATE TABLE IF NOT EXISTS host_current (
    host_id            INT PRIMARY KEY REFERENCES hosts(id) ON DELETE CASCADE,
    collection_id      BIGINT NOT NULL REFERENCES collections(id),
    os_pretty          TEXT,
    kernel             TEXT,
    cpu_model          TEXT,
    cpu_sockets        INT,
    cpu_cores          INT,
    cpu_threads        INT,
    ram_bytes          BIGINT,
    board_vendor       TEXT,
    board_product      TEXT,
    board_serial       TEXT,
    system_vendor      TEXT,
    system_product     TEXT,
    system_serial      TEXT,
    bios_vendor        TEXT,
    bios_version       TEXT,
    primary_mac        TEXT,
    primary_ipv4       TEXT,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Convenience view for "show me everyone".
CREATE OR REPLACE VIEW v_inventory AS
SELECT h.hostname,
       h.label,
       hc.os_pretty,
       hc.kernel,
       hc.cpu_model,
       hc.cpu_sockets,
       hc.cpu_cores,
       hc.cpu_threads,
       pg_size_pretty(hc.ram_bytes)         AS ram,
       hc.system_vendor,
       hc.system_product,
       hc.board_product,
       hc.primary_ipv4,
       hc.updated_at,
       h.last_seen
  FROM hosts h
  LEFT JOIN host_current hc ON hc.host_id = h.id
 ORDER BY h.hostname;
