# buildz — homelab hardware inventory

Collects hardware/OS info from each Linux host over SSH, stores normalized
fields + a raw JSON snapshot in Postgres.

## Pieces

- `collect.sh` — runs on the target host, prints one JSON document. Streamed
  in over SSH; nothing is left behind on the target.
- `cmd/inventory` — Go driver. Reads `hosts.yaml`, ssh's into each host, runs
  the collector, persists to Postgres.
- `schema.sql` — tables: `hosts`, `collections` (raw JSONB, append-only),
  `disks`/`nics`/`memory_modules`/`gpus` (normalized, per-collection),
  `host_current` (latest flat snapshot per host), and a `v_inventory` view.
- `docker-compose.yml` — local Postgres 16 on `127.0.0.1:5433`. Schema is
  applied automatically on first start via `docker-entrypoint-initdb.d`.

## Quick start

```bash
# 1. start postgres
docker compose up -d
# wait until healthy (a few seconds)
docker compose ps

# 2. configure hosts
cp hosts.example.yaml hosts.yaml
$EDITOR hosts.yaml

# 3. build the driver
go build -o inventory ./cmd/inventory

# 4. run
export DATABASE_URL='postgres://buildz:buildz@127.0.0.1:5433/inventory?sslmode=disable'
./inventory -hosts hosts.yaml

# or just one host
./inventory -hosts hosts.yaml -host nas01
```

## Target-host requirements

- SSH key auth from your workstation (`BatchMode=yes` is set; no password prompts).
- Passwordless sudo for the SSH user (`sudo -n bash -s` is used).
- Tools: `bash`, `jq` (required); `lsblk`, `ip`, `dmidecode`, `smartctl`,
  `ethtool`, `lspci` (optional, each section degrades to `null` if missing).

## Inspecting the data

```bash
docker exec -it buildz-pg psql -U buildz -d inventory

inventory=# SELECT hostname, cpu_model, ram, primary_ipv4 FROM v_inventory;
inventory=# SELECT hostname, device, model, pg_size_pretty(size_bytes), tran
              FROM disks d JOIN hosts h ON h.id = d.host_id
             WHERE d.collection_id IN (SELECT collection_id FROM host_current);
```

## Migrating to your real Postgres later

The compose volume `pgdata` is your only state. To move:

```bash
docker exec buildz-pg pg_dump -U buildz -d inventory --format=custom > inventory.dump
# on the target server:
pg_restore -d inventory inventory.dump
```

Or just re-run `inventory` against the new DSN — every collection is
idempotent per-host (host upsert + new collection row + replaced normalized
rows tied to that collection).

## Schema-version note

`collect.sh` stamps `schema_version: 1` into every snapshot. Bump it in the
script *and* teach the driver about the new shape when you change the JSON
contract — old rows in `collections` keep their original schema_version so
re-parsing later remains possible.
