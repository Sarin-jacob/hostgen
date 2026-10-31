# Split DNS for *.jell0.online (hostgen + dnsmasq + Traefik + Tailscale)

Context for working on this repo. Read fully before changing anything.

## Conventions

- Git author is always `sarin-jacob <sarin.c.jacob2001@gmail.com>`. Never commit as Claude.
- Never push to origin. Ask the owner to push.
- No em dashes, en dashes or emojis in code, docs, comments or commit messages.
- The owner is an experienced developer. Ask when something is unclear instead of guessing.
- Prefer small, low resource tools (Go or bash). hostgen is Go, stdlib only, static binary on `scratch`.

## Goal

The same hostnames (`*.jell0.online`) work everywhere with valid certificates:

- On the tailnet (including friends the Tailscale node is shared with), on the LAN and on ZeroTier: names served by Traefik resolve to the local Traefik IP for that network and go straight to Traefik.
- Everything else, and everything when away: names resolve publicly through Cloudflare (proxied records, Cloudflare tunnels, Pages).
- Unknown names should end up at a Cloudflare 404 page (planned: proxied `*.jell0.online` wildcard record serving a "ghost" not found page). Without that record, unknown names return NXDOMAIN.

## Topology

| Thing | Value |
| --- | --- |
| Host | `jell0`, Ubuntu 24.04, Docker, Portainer. Stack files under `/portainer/dns` |
| Wired LAN | `enp0s31f6`, 192.168.1.33. Often DOWN, host then runs on WiFi `wlp1s0` 192.168.1.7 |
| ZeroTier | `ztklh5vshc`, 10.144.25.2 |
| Host Tailscale | `tailscale0`, 100.111.232.103. Host DNS goes through Tailscale (100.100.100.100) |
| Tailscale container | `tailscale_proxy`, node `tailscale-proxy`, 100.101.188.115, tag `tag:container`, kernel mode (`TS_USERSPACE=false`) |
| Docker network | `Cloudy` (external) |
| Owner's global DNS | set to the LAN IP elsewhere; owner does not rely on `dnsmasq-lan` day to day |

Why a separate Tailscale container instead of the host's Tailscale: the `tailscale_proxy` node is shared with friends, who add 100.101.188.115 as the split DNS nameserver for `jell0.online` in their own tailnet. Sharing the container node exposes only it, not the server. SSO on Traefik handles access. Keep this design.

### Request paths

- Tailnet DNS: client, Tailscale split DNS `jell0.online` -> 100.101.188.115:53/udp, Traefik `dns-udp` entrypoint (Traefik shares `tailscale_proxy`'s network namespace), UDP router -> `dnsmasq-ts` on `Cloudy`. Second split DNS nameserver for the same domain: the backup node (see "Backup nameserver").
- LAN DNS: `dnsmasq-lan` published on 192.168.1.33:53/udp.
- ZeroTier DNS: `dnsmasq-zerotier` published on 10.144.25.2:53/udp.
- HTTPS on tailnet: 100.101.188.115:443 -> Traefik (same namespace). Traefik also published as 443/tcp via `tailscale_proxy` ports.
- Certificates: Traefik ACME DNS challenge with Cloudflare, wildcard `*.jell0.online` via the `api` router's `tls.domains`. Other routers get the wildcard through SNI matching. Chain: leaf, Let's Encrypt `YR1`, `Root YR` cross signed by `ISRG Root X1`. Verified OK with openssl.
- Traefik: v3.6, Docker provider only (all routers from labels), `exposedByDefault: false`. Catch-all router `error-router` with `HostRegexp(\`.+\`)` at priority 1 serving `tarampampam/error-pages`.
- Sablier (scale to zero) is used via a Traefik plugin middleware. Containers use `traefik.docker.allownonrunning=true` so Traefik keeps their routers while stopped.

## How DNS answers are decided

dnsmasq has NO wildcard `address=/jell0.online/...` any more. That wildcard sent every name to Traefik, which broke Cloudflare tunnel names unless each one was listed by hand with `server=/name/1.1.1.1`.

Now:

1. `hostgen` reads Docker labels and writes, per dnsmasq instance, a conf file with two lines per Traefik name:
   ```
   host-record=lfs.jell0.online,100.101.188.115
   local=/lfs.jell0.online/
   ```
2. dnsmasq answers those names locally with that instance's IP.
3. Every other name is forwarded to 1.1.1.1 / 1.0.0.1 (`no-resolv`), so tunnel and public names resolve normally.

Why `local=` per name (ECH fix): Cloudflare publishes HTTPS (type 65) records with an ECH key for proxied names. dnsmasq only answers A/AAAA locally and forwards other types, so a client got the local A record plus Cloudflare's ECH key. Firefox based browsers (Zen) then sent an ECH ClientHello to Traefik with outer SNI `cloudflare-ech.com`, Traefik answered with a non matching cert, and Zen showed a certificate warning (seen on `lfs.jell0.online`, which exists on both Traefik and Cloudflare). `local=/name/` stops forwarding for local names, so HTTPS queries for them get no data and the browser does a normal handshake. Tunnel only names keep their full Cloudflare answers, ECH included. Rejected alternatives: global `filter-rr=HTTPS` (needs dnsmasq 2.90+, affects all names), turning off ECH in Cloudflare (zone wide).

Verified on dnsmasq 2.90 (2026-10-31, on dh): a name with `host-record` + `local=` returns NOERROR with an empty answer for HTTPS, its A record locally, and a tunnel only name (`kailo`) keeps Cloudflare's HTTPS record with ECH.

`local=/name/` also covers every subdomain. Bug found 2026-10-31: a router with `Host(\`jell0.online\`)` put the apex in the list, so `local=/jell0.online/` made every tunnel and other machine name NXDOMAIN on dnsmasq-ts (confirmed: `kailo` NXDOMAIN via 100.101.188.115, resolves via 1.1.1.1). Fix: hostgen now also writes `server=/*.name/#` per name, which sends subdomains back to the upstream servers (`#`) while the name itself stays local. The `*.` wildcard needs dnsmasq 2.86 or newer; the `jpillora/dnsmasq` version on jell0 is unknown (open item 7). Older dnsmasq may reject the line or ignore it, so check before deploying, or switch jell0's three dnsmasq containers to the alpine image in `backup/dnsmasq` (2.90, also drops the webproc editor).

Names on both Traefik and Cloudflare are intentional (tailnet when available, internet otherwise). Locally they bypass Cloudflare Access/WAF; Traefik SSO covers that. `EXCLUDE` on hostgen forces a name to always go to Cloudflare.

## hostgen

Go, stdlib only. Files: `main.go`, `main_test.go`, `go.mod` (go 1.21, uses `min`/`max` builtins), `Dockerfile` (golang:1.23-alpine build, `scratch` runtime, no CA certs needed, so `SOURCE` must be plain http over the tailnet), `backup/` (compose for the backup nameserver). About 6 MB binary, about 6.5 MB RSS.

Loop (every `INTERVAL`, default 5s):

1. `GET /containers/json?all=1` over the Docker socket.
2. Keep containers that are running, or stopped/created with `traefik.docker.allownonrunning=true` (Sablier).
3. Skip unless `traefik.enable=true` (matches `exposedByDefault: false`; `EXPOSED_BY_DEFAULT=true` flips it). `traefik.enable=false` always skips.
4. From `traefik.http.routers.*.rule` labels, extract names from `Host(...)` only (backticks or double quotes, multiple args, `||` and `&&` combos). `HostRegexp` is ignored on purpose (the catch-all must not pull every name local). Names are lowercased, trailing dot stripped, validated against `^[a-z0-9_-]+(\.[a-z0-9_-]+)*$` (blocks injection into dnsmasq config).
5. Keep names equal to or under `DOMAIN`, minus `EXCLUDE`.
6. Render one conf per target (`host-record` + `local=` per name, sorted) and write only if content changed, atomically (dot prefixed temp file then rename; dnsmasq `conf-dir` skips dotfiles).
7. If any file changed, queue a restart of every `RESTART` container. dnsmasq never reloads conf files on its own, and even `hostsdir` never drops removed entries without SIGHUP or restart (confirmed in dnsmasq docs and by Simon Kelley on the mailing list), so restart is required.
8. Network namespace watcher (`NETNS_PAIRS=parent:child`): if parent and child are both running and parent `StartedAt` is after child's, queue a restart of child. If child's `NetworkMode` is `container:<id>` for an id that is not the parent's current id or name, the parent was recreated; log once telling the owner to run `docker compose up -d <child>` and do not restart.
9. Run due restarts. Failure: backoff 5s doubling to 5m, error logged once (64 hex Docker ids are normalized so changing endpoint ids do not defeat dedupe). 404 means container missing: skipped and dequeued. Success after failures logs "restarted X after earlier failures".

If Docker cannot be read, files are left untouched. SIGTERM exits 0. `ONCE=1` runs one pass and exits non zero on failure or pending restarts.

Env:

| Var | Meaning |
| --- | --- |
| `DOMAIN` | default `jell0.online` |
| `TARGETS` | required, `path=ip` pairs, comma separated |
| `RESTART` | dnsmasq containers to restart on change (needed) |
| `NETNS_PAIRS` | `tailscale_proxy:traefik` |
| `EXCLUDE` | names never published |
| `EXPOSED_BY_DEFAULT` | default false |
| `INTERVAL` | default 5s, min 1s |
| `DOCKER_SOCK` | default `/var/run/docker.sock` |
| `ONCE` | `1` for a single pass |
| `SERVE` | listen address (`:8053`). `GET /hosts` returns the last successfully read name list, one per line, `#` header line. 503 until the first successful read, so a follower never mistakes "not ready" for "empty" |
| `SOURCE` | `http(s)` URL of another hostgen's `/hosts`. Names come from there instead of Docker labels; Docker is still used for `RESTART` and `NETNS_PAIRS`. Source down, non 200, or any invalid line: whole list rejected, files untouched. `DOMAIN` and `EXCLUDE` apply again |

When the name read fails (Docker or source), `NETNS_PAIRS` checks and due restarts still run, so a backup keeps its dnsmasq healthy while the primary is down.

Tests: `go test -race ./...`. They use a fake Docker API on a unix socket (`/containers/json` requiring `all=1`, `/containers/<name>/json`, `/containers/<name>/restart` with configurable status codes and fresh ids in 500 bodies) and an injectable clock (`app.now`). Covered: label parsing edge cases, Sablier stopped containers, exclude, empty output, unchanged content not rewritten, Docker down keeps files, backoff schedule and cap, single log line for repeated errors, recovery, namespace watcher (healthy, restarted parent, crash looping parent, recreated parent, name based network mode, missing containers), config validation. Not tested against real Docker or real dnsmasq.

## Deployment (`/portainer/dns`)

```
/portainer/dns
├── docker-compose.yml
├── dnsmasq_lan.config
├── dnsmasq_ts.config
├── dnsmasq_zt.config
├── doh-server.conf      # old DoH experiment, unused
├── hostgen/             # main.go, main_test.go, go.mod, Dockerfile
└── hosts/{lan,zt,ts}/   # generated traefik.conf per instance
```

`docker-compose.yml`:

```yaml
services:
  dnsmasq-lan:
    image: jpillora/dnsmasq
    container_name: dnsmasq-lan
    hostname: dns_lan
    ports:
      - "192.168.1.33:53:53/udp"
    volumes:
      - ./dnsmasq_lan.config:/etc/dnsmasq.conf
      - ./hosts/lan:/etc/dnsmasq.hosts.d:ro
    restart: unless-stopped

  dnsmasq-zerotier:
    image: jpillora/dnsmasq
    container_name: dnsmasq-zerotier
    hostname: dns_zt
    ports:
      - "10.144.25.2:53:53/udp"
    volumes:
      - ./dnsmasq_zt.config:/etc/dnsmasq.conf
      - ./hosts/zt:/etc/dnsmasq.hosts.d:ro
    restart: unless-stopped

  dnsmasq-ts:
    image: jpillora/dnsmasq
    container_name: dnsmasq-ts
    hostname: dns_ts
    networks: [Cloudy]
    volumes:
      - ./dnsmasq_ts.config:/etc/dnsmasq.conf
      - ./hosts/ts:/etc/dnsmasq.hosts.d:ro
    restart: unless-stopped
    labels:
      - "traefik.enable=true"
      - "traefik.udp.routers.dns-router.entrypoints=dns-udp"
      - "traefik.udp.services.dns-service.loadbalancer.server.port=53"

  dns-hostgen:
    build: ./hostgen
    image: hostgen:local
    container_name: dns-hostgen
    restart: unless-stopped
    read_only: true
    environment:
      DOMAIN: jell0.online
      TARGETS: >-
        /out/lan/traefik.conf=192.168.1.33,
        /out/zt/traefik.conf=10.144.25.2,
        /out/ts/traefik.conf=100.101.188.115
      RESTART: dnsmasq-lan,dnsmasq-zerotier,dnsmasq-ts
      NETNS_PAIRS: tailscale_proxy:traefik
      SERVE: ":8053"
      # EXCLUDE: some.jell0.online
    ports:
      # read by the backup over the tailnet (100.111.232.103:8053). Bound on
      # all interfaces: binding to the tailscale IP fails the whole container
      # when tailscale0 is not up yet. The list holds the same names as the
      # public DNS records, so LAN exposure is harmless.
      - "8053:8053/tcp"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - ./hosts/lan:/out/lan
      - ./hosts/zt:/out/zt
      - ./hosts/ts:/out/ts

networks:
  Cloudy:
    external: true
```

Each `dnsmasq_*.config` (no `address=` wildcard, no manual `server=/x/` lines):

```
cache-size=1000
no-resolv
server=1.1.1.1
server=1.0.0.1
conf-dir=/etc/dnsmasq.hosts.d,*.conf
log-queries
```

Old `traefik.hosts` files in `hosts/*` are ignored by the `*.conf` filter and can be deleted.

Traefik / Tailscale stack (separate compose, summarized): `traefik:v3.6` with `network_mode: "service:tailscale_proxy"`, Docker socket, `traefik.yml`, Cloudflare DNS API token env. `tailscale/tailscale:latest` with `TS_STATE_DIR`, auth key, `--advertise-tags=tag:container`, `NET_ADMIN`, `SYS_MODULE`, `/dev/net/tun` mounted as a volume, `Cloudy` network, port `443:443/tcp`. `error-pages` on `Cloudy`.

Deploy: `docker compose up -d --build` in `/portainer/dns`. If deployed from Portainer's web editor, relative paths resolve in Portainer's data dir; use absolute `/portainer/dns/...` paths then.

## Backup nameserver

Problem: jell0 (the host) goes down often. Split DNS for `jell0.online` then has no working nameserver, so on the tailnet every `.jell0.online` name fails, including tunnel, Pages and services on other machines. Friends are affected the same way.

How Tailscale uses several nameservers for one split domain (read in `net/dns/resolver/forwarder.go`, `forwardWithDestChan`): it queries all of them at once and returns the first successful answer. NOERROR and NXDOMAIN count as success; SERVFAIL and REFUSED are soft errors, it waits for the others. So a second nameserver is safe only if it gives the same answers as jell0.

Design: a second node whose dnsmasq serves the same conf. hostgen on jell0 serves its name list (`SERVE`), a hostgen on the backup follows it (`SOURCE`) and renders it with jell0's tailnet IP 100.101.188.115, keeping the last good list while jell0 is down. Answers are identical whichever node replies first, so there is no race. When jell0 is down, Traefik names point at a dead IP (those services are down anyway) and every other name resolves through 1.1.1.1.

Where: host `dh` (x86_64, Docker 29, ssh alias `dh`, user `dh`), stack in `/home/dh/sarin/docker/jell0dns` (a copy of `backup/` with the hostgen source in `./hostgen`, built there; re-copy `main.go` after hostgen changes). dh is in another person's tailnet and stays there; the `jell0dns-tailscale` container is a separate node (`jell0-dns-backup`, `tag:container`) in the owner's tailnet, shared with friends like `tailscale-proxy`. dnsmasq and hostgen run in its network namespace. dnsmasq is plain alpine dnsmasq, not `jpillora/dnsmasq`, whose webproc editor on 8080 would be reachable by shared users.

Setup:
1. jell0: add `SERVE` and the `8053` port to `dns-hostgen` (see compose above), redeploy, check `curl http://100.111.232.103:8053/hosts` from a tailnet device.
2. Admin console: create a tagged auth key (`tag:container`). If the tailnet policy is not allow all, allow `tag:container` to `100.111.232.103:8053` (tcp), and keep whatever lets sharees reach `tailscale-proxy` on 53 working for the new node too.
3. dh: in `~/sarin/docker/jell0dns`, write `.env` with `TS_AUTHKEY=...`, run `docker compose up -d --build`. Check `docker logs jell0dns-hostgen` shows `wrote /out/traefik.conf`.
4. Admin console: share `jell0-dns-backup` with each friend. Owner and each friend (once, in the web UI): DNS, Add nameserver, Custom, the backup node's 100.x IP, Restrict to domain `jell0.online`.

Rejected alternatives: 1.1.1.1 as second nameserver (Tailscale sends DoH to Cloudflare immediately, it races and wins for local names); a delayed fallback forwarder (needs the same machine anyway, and timing based); Cloudflare Worker DoH (Tailscale only dials DoH URLs from its built in provider list, or with `bootstrapResolution`, which is in an alpha, account manager gated policy feature); cloning `tailscale_proxy` state onto a standby (unsupported, two instances fight); public grey records pointing at the tailnet IP (public A records must stay on the LAN IP for apps that ignore local DNS).

Known risk: Tailscale issue #12677 reports iOS/macOS not failing over cleanly when one of several nameservers is offline. Test with jell0 off on an Apple device before relying on it.

## Incidents and findings (October 2026)

1. **Traefik stranded in a dead network namespace.** `tailscale_proxy` restarted (RestartCount 2996 since creation on 2026-10-22, last exit code 0, no OOM, restarted 76 ms later, nothing in its logs for those restarts). Traefik stayed in the old namespace, so 100.101.188.115 refused 53 and 443 and the whole split domain broke for everyone, including friends (split DNS has no fallback). Fixed with `docker restart traefik`. Prevention: hostgen `NETNS_PAIRS`. Owner believes the restarts happen when the internet drops; loop not active when last checked.
2. **Host DNS outages.** During the incident, Docker's embedded DNS (forwarding to systemd-resolved 127.0.0.53, which uses `tailscale0` 100.100.100.100) timed out for everything (`controlplane.tailscale.com`, `argotunnel.com`, ...). That breaks cloudflared and Tailscale in containers. Owner chose not to change `resolved.conf`. Optional hardening: `dns: [1.1.1.1, 1.0.0.1]` on `tailscale_proxy` and cloudflared.
3. **`dnsmasq-lan` cannot start while `enp0s31f6` is down** (`failed to bind host port for 192.168.1.33:53 ... cannot assign requested address`). Harmless for the owner. hostgen backs off. Optional: `sysctl net.ipv4.ip_nonlocal_bind=1` (persist in `/etc/sysctl.d/`) so the bind succeeds before the address exists.
4. **Sablier middlewares invalid** (`invalid middleware "kiwix-sablier@docker" configuration: invalid middleware type or middleware does not exist`). Traefik downloads plugins from `plugins.traefik.io` at startup; if that fails (no DNS/internet at the moment of restart), all plugins are disabled for that run. Check `docker logs traefik 2>&1 | grep -i plugin`. Fix: restart Traefik when online. The namespace watcher restarts Traefik right after Tailscale returns, often when connectivity is flaky, so moving Sablier to `experimental.localPlugins` is the real fix (see open items).
5. **Zen certificate warning on `lfs.jell0.online`**: ECH, see "How DNS answers are decided".

## Open items

1. NODATA behaviour verified on dnsmasq 2.90 (see above). Still to do: confirm Zen opens `lfs.jell0.online` on the tailnet (clear `about:networking#dns` first), on jell0's dnsmasq version.
2. Sablier as a local plugin: move from `experimental.plugins` to `experimental.localPlugins` keeping the same `moduleName`, clone the plugin repo at the current version into `./plugins-local/src/<moduleName>`, mount `./plugins-local:/plugins-local:ro` into Traefik. Middleware labels stay the same. Need the current `experimental` section of `traefik.yml`.
3. Log rotation on all these services (`log-queries` is chatty, default json-file never rotates):
   ```yaml
   logging:
     driver: json-file
     options: { max-size: "5m", max-file: "2" }
   ```
4. Cloudflare: proxied `*.jell0.online` wildcard record serving the 404 ghost page.
5. Optional: decouple Traefik from Tailscale with containerboot `TS_DEST_IP` (iptables DNAT of tailnet traffic to a static Traefik IP on `Cloudy`), keeping the shared node and friends' nameserver IP unchanged. Unverified: whether Traefik would see real client IPs.
6. Optional: merge `dnsmasq-lan` and `dnsmasq-zerotier` into one host networked dnsmasq with `bind-dynamic` and `localise-queries`. `dnsmasq-ts` must stay for the shared node.
7. Optional: check the jpillora/dnsmasq version (`docker exec dnsmasq-ts dnsmasq -v | head -1`); it may be old. Configs only use long standing options.
8. Optional: `traefik.tcp.routers.*` with `HostSNI` are not read by hostgen (none in use).
9. Backup nameserver: deployed on dh 2026-10-31, node `jell0-dns-backup` 100.64.136.114. First fetch from jell0 worked (37 names), later TCP from the node to 100.111.232.103 times out on every port while `tailscale ping` works: tailnet policy must allow `tag:container` to `100.111.232.103:8053`. Then test with jell0 off: `dig @<backup 100.x> kailo.jell0.online +short` and a browser on a tailnet device, including an iOS/macOS one. Not yet tested against real Docker, Tailscale or dnsmasq.

## Useful commands

```
docker logs --tail 50 dns-hostgen
docker ps -a --format '{{.Names}}\t{{.Status}}' | grep -E 'dnsmasq|traefik|tailscale|hostgen'
docker inspect -f '{{.RestartCount}} {{.State.StartedAt}}' tailscale_proxy
openssl s_client -connect 100.101.188.115:443 -servername <name>.jell0.online -verify_return_error </dev/null 2>&1 | grep -E 'depth|Verify return'
IP=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' dnsmasq-ts); dig @$IP <name>.jell0.online
cd hostgen && go vet ./... && go test -race ./...
```
