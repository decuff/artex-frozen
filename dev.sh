#!/usr/bin/env bash
# Dev mode: backend (:8787) + traffic proxy (:8788) run together with frontend next dev (:5173).
# Frontend /api reverse-proxies to backend; Ctrl-C exits both.
#
# For the single-binary (frontend embedded) approach see the README 'Single Binary' section; it doesn't use this script.
set -euo pipefail
cd "$(dirname "$0")"

# On exit, terminate all child processes in this process group (backend + frontend).
cleanup() { kill 0 2>/dev/null || true; }
trap cleanup EXIT INT TERM

# Backend (plain go run, frontend not embedded); the number of concurrent work agents is configured in 'System Settings'.
go run ./cmd/artex -addr :8787 -proxy 127.0.0.1:8788 &

# Frontend hot reload (Vite/Next dev server, /api reverse-proxied to :8787).
( cd web && npm run dev ) &

echo "[dev] backend :8787 / proxy :8788 / frontend http://localhost:5173  (Ctrl-C to exit)"
wait
