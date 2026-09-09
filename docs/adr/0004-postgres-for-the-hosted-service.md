---
status: accepted
date: 2026-09-09
---

# Run the hosted service on Postgres, not SQLite

The self-hosted server defaults to SQLite, and that default stays. The hosted, multi-tenant service runs on Postgres from its first deployment: FUTO's Patroni cluster via Manifest for the internal MVP, CloudNativePG when it moves onto Yucca's Kubernetes cluster. Blobs stay on the filesystem for the MVP; an S3 blob store against Yucca's Ceph RGW is the follow-up that completes the move.

## Considered options

- SQLite on one box with a host volume. Rejected because the destination infrastructure is Postgres and S3, we ship a Postgres-to-SQLite converter but no reverse, and backups, replication, and failover would have to be invented for a paid service that already has them on Postgres. The SQLite latency win measured in `docs/Resource comparison, TypeScript vs Go.md` is a self-hoster benefit, not something a device behind HAProxy notices.
- Postgres plus S3 from day one. Deferred, not rejected: Yucca's Ceph S3 is staging-only today and the blob store does not exist yet.

## Consequences

- Both engines stay supported and tested; hosted work must not regress SQLite.
- Number 0003 is reserved for the folder-subtree sharing ADR that lives on the architecture-direction branch and is not yet on `main`.
