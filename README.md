# ASPanel

ASPanel is a security automation panel that connects AWVS scanning, sqlmap exploitation,
path discovery, proxy routing, cloud node management, and local result caching into one
workflow. It is designed for controlled, authorized testing environments.

## Core Workflow

1. Import or create target URLs in the panel.
2. Dispatch targets to available AWVS nodes with concurrency balancing.
3. Collect confirmed SQL injection findings from AWVS.
4. Send SQLi findings to sqlmap agents for validation, enumeration, and limited data dump.
5. Normalize sqlmap output into structured SQLite records and domain-level cache.
6. Search cached/exported results globally by hash, keyword, database, table, column, or row data.

## Components

- **Panel backend**: Go, Gin, GORM, SQLite.
- **Panel frontend**: React, TypeScript, Vite, TanStack Query, Ant Design.
- **AWVS node manager**: lightweight Docker manager for health checks, updates, restarts, uninstall, and hard reinstall.
- **sqlmap agent**: Python service that wraps sqlmap tasks and converts outputs into structured snapshots.
- **Path agent**: optional path/form discovery worker.
- **Proxy agent**: xray/VLESS-style proxy routing for sqlmap traffic.
- **Cloud integration**: Tencent Cloud spot/on-demand node launch, cleanup, reboot, and record binding.

## Structured sqlmap Data

The project does not rely on sqlmap's internal session files as a complete target database.
Instead, the agent drives sqlmap with explicit structural actions and the panel stores the
result in SQLite:

- `--dbs` -> `content.dbs`
- `-D <db> --tables` -> `content.tables`
- `-D <db> -T <table> --columns` -> `content.columns`
- `--dump --dump-format=SQLITE` -> `dump_files` and row previews
- normalized tree -> `tree.databases[].tables[].columns[]`
- domain merge cache -> `DomainSQLMapCache.ContentJSON` and `DomainSQLMapCache.TreeJSON`

This makes the UI tree, global hash search, and priority-table selection depend on verified
enumeration results instead of ambiguous text output.

## sqlmap `--search` Policy

Automatic use of sqlmap `--search` is currently disabled.

Reason: `--search` is a keyword search feature, not a reliable structural enumeration source.
In practice, search output can look like database/table metadata and accidentally pollute the
panel's `dbs`, `tables`, `tree`, and domain cache. That breaks later global hash lookup because
the UI may show search hits as if they were real database names.

Current strategy:

- Slow targets still continue through structured enumeration.
- Slow or blind techniques use conservative settings such as `threads=1`.
- Search results from existing cached structured data are still supported in the panel.
- Future `--search` support should store results as separate unverified hints, then verify them
  with `--tables`, `--columns`, or `--dump` before writing to the main tree.

## Priority Table Strategy

After table and column enumeration, the system prioritizes credential-bearing tables:

- Password-like columns: `password`, `passwd`, `pwd`, `pass`, `pass_hash`, `password_hash`.
- Admin-like table names: `admin`, `administrator`, `manager`, `staff`, `root`, and related names.
- Tables with verified password-like columns are preferred over tables that only look sensitive by name.

This priority is used when choosing which tables to enumerate or dump first.

## Global Export Search

Global Export Search searches panel-side SQLite cache, not live remote agents. It covers:

- task-level sqlmap snapshots
- finding-level sqlmap snapshots
- domain-level merged cache

The search runs as a background task to avoid Cloudflare 524 timeouts. The UI shows a task ID,
polls status, and allows loading historical search tasks later.

## AWVS Node Maintenance

AWVS nodes expose manager health data back to the panel, including disk usage. The panel can:

- mark a node as draining when disk is low
- stop sending new scans to draining/updating nodes
- wait for bound scans to finish
- trigger a hard AWVS reinstall through the node manager
- parse the new `awvsagent://` registration link from manager logs
- update the existing AWVS record with the new API key and credentials

Default low-disk thresholds:

- used percent >= `85%`
- free space <= `10GB`

These values are configurable per AWVS node.

## Immutable File Handling

AWVS uninstall and hard reinstall scripts clear Linux immutable/append-only attributes before
removing container data:

- `chattr -R -i -a`
- common AWVS paths under `/home/acunetix`, `/opt/acunetix`, `/var/lib/acunetix`, and Docker graph paths

This handles installations that mark certificate or state files with `chattr +i`.

## Background Tasks

Long-running panel work is moved out of request/response paths where practical:

- global sqlmap export search
- AWVS cleanup
- AWVS low-disk drain and hard reinstall
- cloud lifecycle operations
- sqlmap/path task status sync

This avoids reverse proxy timeouts and keeps the UI recoverable after refresh.

## Release Notes For Current Behavior

- Automatic sqlmap `--search` is disabled.
- Slow sqlmap targets continue structured enumeration instead of falling back to keyword search.
- Global Export Search is asynchronous and returns a durable task ID.
- AWVS low-disk remediation can hard reinstall nodes and reconnect them automatically.

## Development

Backend:

```bash
go test ./...
```

Frontend:

```bash
cd frontend
npm install
npm run build
```

sqlmap agent:

```bash
python -m py_compile sqlmap_agent.py
```

