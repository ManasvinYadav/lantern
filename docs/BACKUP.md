# Backup & Restore

Lantern stores everything — service history, groups, webhook configs, active
monitors, and settings — in a single SQLite file. Backing up and restoring
the whole dashboard is just backing up and restoring that file.

## Creating a backup

**From the UI:** open **Settings → Backup → Download Backup**. This downloads
a `lantern-backup-<timestamp>.db` file.

**From the API:**

```bash
curl -o lantern-backup.db http://localhost:7654/api/backup \
  -H "Authorization: Bearer your_token_here"
```

`GET /api/backup` requires authentication as of v0.60.0. Pass a bearer token as
above, or a session cookie if you are scripting against a signed-in session.

Either method runs SQLite's `VACUUM INTO`, which produces a single
consistent snapshot file even while Lantern is running and accepting writes
— there's no need to stop the container first, and no risk of grabbing a
half-written WAL file mid-checkpoint.

## Restoring a backup

Restoring means replacing the live database file with the backup, so it
requires a brief restart:

```bash
# 1. Stop Lantern
docker compose down

# 2. Copy the backup over the live database.
#    The default path inside the named volume is /data/lantern.db — find the
#    volume's real path on the host with:
docker volume inspect lantern_lantern_data --format '{{.Mountpoint}}'

#    Then, as root (Docker volumes are typically root-owned):
sudo cp lantern-backup.db /var/lib/docker/volumes/lantern_lantern_data/_data/lantern.db

# 3. Remove any leftover WAL/SHM files from the previous run so SQLite
#    doesn't try to replay a WAL that no longer matches the restored file.
sudo rm -f /var/lib/docker/volumes/lantern_lantern_data/_data/lantern.db-wal \
           /var/lib/docker/volumes/lantern_lantern_data/_data/lantern.db-shm

# 4. Start Lantern back up
docker compose up -d
```

If you're running Lantern outside Docker (a bare `LANTERN_DB_PATH`), the
same idea applies: stop the process, overwrite the file at `LANTERN_DB_PATH`
(and remove any `-wal`/`-shm` siblings), then start it again.

## What's included

Everything stored in the database:

- status history, subject to `LANTERN_RETENTION_DAYS`
- diagnostic runs
- service groups
- maintenance state and maintenance windows
- saved webhook URLs and the webhook delivery history
- active monitor configurations, including captured TLS certificate expiry dates
- every account (username, bcrypt password hash, role) and any per-service API tokens

## What's not included

Anything that lives outside the SQLite file. In particular your environment —
`LANTERN_AUTH_TOKEN`, `LANTERN_AUTH_USER`/`PASS`, the `LANTERN_WEBHOOK_*`
variables — and your `docker-compose.yml` or `.env`. Back those up separately if
they hold values you would need to reconstruct.

## Treat the snapshot as a secret

The file is a complete copy of the database, so it contains the bcrypt hash of
your admin password, the SHA-256 hashes of every live session token, your
per-service API tokens, and any webhook URLs saved through the UI — and a
Discord or Telegram webhook URL is itself a credential.

Store backups with the same care as the credentials they contain: not in a
public repository, not in a world-readable directory, and not in an object
bucket you have not locked down. Before v0.60.0 this endpoint was reachable
without a credential when only `LANTERN_AUTH_TOKEN` was configured; it is
authenticated now, but any snapshot you took earlier is still as sensitive as
its contents.
