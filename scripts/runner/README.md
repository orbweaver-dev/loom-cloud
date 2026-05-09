# Self-hosted GitHub Actions runner — loom-cloud on wh1

Parallel to `loom`'s runner setup, but registers under a
**separate user** (`loom-cloud-runner`) so the two runners
don't share working directories or credentials. Same security
posture: dedicated user, no sudo, no SSH keys, no FrothIQ
webroot access.

## Why a separate user

A self-hosted runner executes whatever code lands in a PR.
Sharing one user between two repos means a malicious PR in
either repo can read the other repo's checked-out code,
test artifacts, and any cached secrets. Two users → two
home directories → smaller blast radius.

## One-time setup on wh1

```bash
sudo useradd -m -s /bin/bash loom-cloud-runner
sudo usermod -aG docker loom-cloud-runner

sudo -u loom-cloud-runner -i
export GITHUB_PAT='github_pat_...'
curl -O https://raw.githubusercontent.com/orbweaver-dev/loom-cloud/main/scripts/runner/runner-setup.sh
chmod +x runner-setup.sh
./runner-setup.sh
exit

sudo /home/loom-cloud-runner/actions-runner-loom-cloud/svc.sh install loom-cloud-runner
sudo /home/loom-cloud-runner/actions-runner-loom-cloud/svc.sh start
sudo systemctl status actions.runner.orbweaver-dev-loom-cloud.wh1-loom-cloud
```

## Operations

| Task | Command (run as root on wh1) |
|---|---|
| Stop | `sudo /home/loom-cloud-runner/actions-runner-loom-cloud/svc.sh stop` |
| Restart | `sudo /home/loom-cloud-runner/actions-runner-loom-cloud/svc.sh start` |
| Logs | `sudo journalctl -u actions.runner.orbweaver-dev-loom-cloud.wh1-loom-cloud -f` |
| Unregister | `sudo -u loom-cloud-runner /home/loom-cloud-runner/actions-runner-loom-cloud/config.sh remove --token <removal-token>` |

## Security checklist

Same as loom's, with the loom-cloud-runner user substituted:

- [ ] `loom-cloud-runner` is **not** in the `sudo` / `wheel` groups
- [ ] `loom-cloud-runner` does **not** have your `~/.ssh/id_ed25519` or any deploy keys
- [ ] FrothIQ-protected sites' webroots are **not** writable by `loom-cloud-runner`
- [ ] Repo settings → Actions → "Require approval for first-time contributors" is enabled
