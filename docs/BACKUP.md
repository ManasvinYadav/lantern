# Backup & Restore

Lantern stores everything — service history, groups, webhook configs, active
monitors, and settings — in a single SQLite file. Backing up and restoring
the whole dashboard is just backing up and restoring that file.

## Creating a backup

**From the UI:** open **Settings → Backup → Download Backup**. This downloads
a `lantern-backup-<timestamp>.db` file.

**From the API:**

```bash
curl -o lantern-backup.db http://localhost:7654/api/backup
```

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

Everything in the database: status history (subject to
`LANTERN_RETENTION_DAYS`), diagnostic runs, service groups, maintenance
windows and state, saved webhook URLs, webhook delivery history, and active
monitor configurations (including captured TLS certificate expiry dates).

Nothing outside the database — environment variables, the Docker Compose
file itself, and API tokens issued via `api_tokens` are part of the DB and
so are included, but your `.env` file (if you use one) is not; keep that
backed up separately if it holds secrets you'd need to reconstruct.
