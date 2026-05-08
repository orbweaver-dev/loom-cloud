# loom-cloud

The hosted Loom runtime. Per-tenant `<slug>.loom.dev` deployments,
container provisioning, DNS / TLS termination, billing hooks.

This is a **separate repo from `loom`** because Loom Core is
*immutable to third-party apps* — apps installed on Loom must
extend Core through interfaces, hooks, config, and composition,
never by editing source. loom-cloud is itself an app on top of
Loom; it composes the framework's hosting primitives
(`pkg/hosting`) into a working hosted product.

## Status

- **v0.0.1** — scaffold. Site Thread, Docker provisioner skeleton,
  composes `pkg/hosting.SubdomainResolver` for per-tenant routing.
  Not production-ready; no real edge router, no DNS automation,
  no billing.

## Architecture

```
┌──────────────────────────┐
│  loom-cloud edge router  │  *.loom.dev → SubdomainResolver → tenant
│  (this repo)             │
└─────────────┬────────────┘
              │ proxies to per-tenant deployment
              ▼
┌──────────────────────────┐
│  Tenant's Loom app       │  built from their YAMLs +
│  (their repo + binary)   │  imports loom-cloud's runtime hooks
└──────────────────────────┘
```

The "tenant deployment" is a container the Provisioner spins up
when a Site row goes from Pending → Provisioning → Running. The
Provisioner interface is in `pkg/hosting`; this repo ships a
Docker-based reference implementation in `internal/docker/`.

## Layout

```
loom-cloud/
├── apps/
│   └── cloud/                # the loom-cloud app shipped with the runtime
│       ├── threads/
│       │   └── site.yaml     # Site Thread (per-tenant deployment row)
│       └── main.go           # binary entry point
├── internal/
│   ├── docker/               # DockerProvisioner reference impl
│   ├── edge/                 # edge router (subdomain routing → proxy)
│   └── seedflow/             # bootstrap flow (Site = Pending → Running)
├── go.mod                    # requires github.com/orbweaver-dev/loom
└── README.md
```

## Loom Core dependency

This repo always imports a tagged release of `github.com/orbweaver-dev/loom`,
never main. Bumping the dependency is a deliberate operation that
should run the loom-cloud test suite against the new tag.

## Roadmap

- [x] **v0.0.1** — Repo scaffold, Site Thread, Docker provisioner
      skeleton, edge router skeleton with `pkg/hosting.SubdomainResolver`.
- [ ] **v0.1.0** — DNS automation (Route 53 / Cloudflare provider
      interface), TLS via certbot.
- [ ] **v0.2.0** — Billing hooks via `pkg/shuttle/stripe`
      (subscription lifecycle → Site status transitions).
- [ ] **v0.3.0** — Backups (per-tenant database snapshots, scheduled
      via `App.ScheduledJobs`).
- [ ] **v0.4.0** — Branch deploys (PR previews on `<slug>-<branch>.loom.dev`).
- [ ] **v1.0.0** — Production-ready: K8s provisioner alongside Docker,
      multi-region, observability.

## License

Same as loom (TBD; default Apache-2 unless stated otherwise).
