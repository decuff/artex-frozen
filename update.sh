#!/usr/bin/env bash
# ARTEX update script: ① Docker update (pull new image and rebuild)  ② Local build update (rebuild binary)
# Corresponds to install.sh: install handles first-time setup, update handles upgrading to a new version.
# DB migration requires no manual execution—artex idempotently re-runs schema.sql on every startup (including ADD COLUMN/CREATE
# INDEX IF NOT EXISTS), so 'restart means migrate'. Data (pgdata volume, ./data, ./skills) is unaffected.
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }

# ── Optional: sync repo to latest code (compose/scripts/local build source all rely on this to update)───────
sync_repo(){
  [ -d .git ] && command -v git >/dev/null 2>&1 || { warn "Not a git working copy, skipping git pull"; return; }
  [ "$(ask 'Pull latest code (git pull --ff-only)? (y/n)' y)" = y ] || return
  if ! git pull --ff-only; then
    warn "git pull could not fast-forward (local changes or diverged branch) — please resolve manually and retry; continuing with current code"
  fi
}

# ── ① Docker update ───────────────────────────────
update_docker(){
  command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1 \
    || die "docker / docker compose not detected, please install and deploy with ./install.sh first"
  [ -f .env ] || die ".env not found, please run ./install.sh first to complete the initial deployment"

  # Optional: upgrade to a specific version tag (if left blank, use ARTEX_TAG from .env, defaulting to latest)
  local tag; tag="$(ask 'Target image tag (Enter to use .env / latest)' '')"
  if [ -n "$tag" ]; then
    if grep -q '^ARTEX_TAG=' .env; then
      sed -i.bak "s|^ARTEX_TAG=.*|ARTEX_TAG=${tag}|" .env && rm -f .env.bak
    else
      printf '\nARTEX_TAG=%s\n' "$tag" >> .env
    fi
    ok "Set ARTEX_TAG to ${tag}"
  fi

  # Only touch artex: postgres is fixed at 16-alpine and doesn't need to be upgraded along (pulling it is a pure waste of bandwidth,
  # and major version changes carry compatibility risk). artex declares depends_on postgres, so when bringing up with the service name
  # if pg isn't running it will be started automatically; if already running it's kept as-is, not rebuilt.
  info "Pulling new image (artex only)…"
  docker compose pull artex
  info "Rebuilding and starting (schema auto-migrates when artex restarts)…"
  docker compose up -d artex
  ok "Update complete → http://localhost:8787"
  info "View logs: docker compose logs -f artex"
  info "Clean up old images (optional): docker image prune -f"
}

# ── ② Local build update ──────────────────────────────
update_local(){
  command -v go >/dev/null 2>&1 || die "Go (>=1.26) not detected: https://go.dev/dl/"
  [ -f config.json ] || warn "config.json not found — if this is the first deployment, use ./install.sh instead"
  ok "Go: $(go version)"

  if command -v npm >/dev/null 2>&1; then
    info "Rebuilding frontend static assets…"
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "Recompiling embedded single binary…"
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm not detected: compiling the backend **without embedding the frontend** (frontend must be run separately with npm run dev)"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "Build complete → ./artex"
  warn "Please restart the running artex process for changes to take effect (schema will be auto-migrated on restart)"
}

echo "=============================="
echo "  ARTEX Update"
echo "  1) Docker update (pull new image and rebuild)"
echo "  2) Local update (recompile with go)"
echo "=============================="
case "$(ask 'Select' 1)" in
  1) sync_repo; update_docker ;;
  2) sync_repo; update_local ;;
  *) die "Invalid selection" ;;
esac
