# Self-hosted GitHub Actions runner — loom-cloud on beast64

Parallel to `loom`'s runner setup, but registers under a
**separate user** (`loom-cloud-runner`) so the two runners
don't share working directories or credentials. Same security
posture: dedicated user, no sudo, no SSH keys, no access to
other users' working trees.

## Why a separate user

A self-hosted runner executes whatever code lands in a PR.
Sharing one user between two repos means a malicious PR in
either repo can read the other repo's checked-out code,
test artifacts, and any cached secrets. Two users → two
home directories → smaller blast radius.

## One-time setup on beast64

```bash
sudo useradd -m -s /bin/bash loom-cloud-runner
sudo usermod -aG docker loom-cloud-runner

sudo -u loom-cloud-runner -i
export GITHUB_PAT='github_pat_...'
curl -O https://raw.githubusercontent.com/orbweaver-dev/loom-cloud/main/scripts/runner/runner-setup.sh
chmod +x runner-setup.sh
./runner-setup.sh
exit

cd /home/loom-cloud-runner/actions-runner-loom-cloud
sudo ./svc.sh install loom-cloud-runner
sudo ./svc.sh start
sudo systemctl status actions.runner.orbweaver-dev-loom-cloud.beast64-loom-cloud
```

## Operations

| Task | Command (run as root on beast64) |
|---|---|
| Stop | `sudo /home/loom-cloud-runner/actions-runner-loom-cloud/svc.sh stop` |
| Restart | `sudo /home/loom-cloud-runner/actions-runner-loom-cloud/svc.sh start` |
| Logs | `sudo journalctl -u actions.runner.orbweaver-dev-loom-cloud.beast64-loom-cloud -f` |
| Unregister | `sudo -u loom-cloud-runner /home/loom-cloud-runner/actions-runner-loom-cloud/config.sh remove --token <removal-token>` |

## Security checklist

- [ ] `loom-cloud-runner` is **not** in the `sudo` / `wheel` groups
- [ ] `loom-cloud-runner` cannot read `~adrian/.ssh/id_ed25519` or any other user's secrets (Fedora default home perms enforce this)
- [ ] Repo settings → Actions → "Require approval for first-time contributors" is enabled
