# syntax=docker/dockerfile:1
#
# Run the image (no compilation inside the image): only install common tools, drop in the **prebuilt Linux single binary**.
# The binary is cross-compiled by the CI binaries job (pure Go, no QEMU), placed by target architecture in
# the build context at dist/<TARGETARCH>/artex. This way, for multi-arch builds arm64 only needs to emulate the apt layer,
# no longer emulating Next/Go compilation, which is much faster.
#
# When building the image manually locally, prepare the binary yourself first:
#   cd web && npm run build:static && cd ..
#   cp -r web/out server/webui/dist
#   CGO_ENABLED=0 GOARCH=amd64 go build -tags embedui -o dist/amd64/artex ./cmd/artex
#   docker build -t artex:local .
FROM python:3.12-slim-bookworm
ARG TARGETARCH
# Common tools: ripgrep / curl / vim, plus a batch of recon essentials (add or remove as needed).
# Install Node 20.x from NodeSource: bookworm's bundled apt nodejs is 18, and Playwright requires >=20.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*
# Preinstall Playwright MCP and CLI (globally), so no npx online download at runtime.
# @playwright/mcp: browser MCP directly via `npx @playwright/mcp` (already installed globally, no -y/@latest needed).
# @playwright/cli: provides playwright-cli, and after install run --help to verify it is executable.
# Then install playwright (provides browser management), and after install use --with-deps to preset chromium and its system dependencies,
# so that MCP/CLI inside the container works on first launch, with no online browser download.
RUN npm install -g @playwright/mcp@latest @playwright/cli@latest playwright@latest \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
# Prebuilt binary for the corresponding architecture (dist/amd64/artex or dist/arm64/artex)
COPY dist/${TARGETARCH}/artex /app/artex
# Daemon startup script: after the process exits, decide by exit code whether to relaunch; one-click page updates rely on it to swap in the new build.
# It is also responsible for forwarding SIGTERM to artex — docker stop only sends the signal to PID 1,
# and without forwarding, artex won't receive it and can't do a graceful shutdown, getting hard-killed by SIGKILL after 10 seconds.
COPY start.sh /app/start.sh
RUN chmod +x /app/artex /app/start.sh
COPY skills/ /app/skills/
# data/ (SQLite + jwt.key) persistence point
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
