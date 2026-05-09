#!/usr/bin/env bash
#
# runner-setup.sh — register a GitHub Actions self-hosted runner
# for orbweaver-dev/loom-cloud on this machine (typically wh1).
#
# Parallel to scripts/runner/runner-setup.sh in the loom repo
# but registers a SEPARATE runner under a DIFFERENT user
# (loom-cloud-runner) so the two runners don't share a working
# directory or credentials. Same security posture: dedicated
# user, no sudo, no SSH keys, no FrothIQ webroot access.
#
# Usage on wh1:
#   sudo useradd -m -s /bin/bash loom-cloud-runner
#   sudo usermod -aG docker loom-cloud-runner
#   sudo -u loom-cloud-runner -i
#   export GITHUB_PAT='github_pat_...'
#   curl -O https://raw.githubusercontent.com/orbweaver-dev/loom-cloud/main/scripts/runner/runner-setup.sh
#   chmod +x runner-setup.sh
#   ./runner-setup.sh
#   exit
#   sudo /home/loom-cloud-runner/actions-runner-loom-cloud/svc.sh install loom-cloud-runner
#   sudo /home/loom-cloud-runner/actions-runner-loom-cloud/svc.sh start
set -euo pipefail

REPO_OWNER="${REPO_OWNER:-orbweaver-dev}"
REPO_NAME="${REPO_NAME:-loom-cloud}"
REPO_URL="${REPO_URL:-https://github.com/${REPO_OWNER}/${REPO_NAME}}"
RUNNER_VERSION="${RUNNER_VERSION:-2.319.1}"
RUNNER_DIR="${RUNNER_DIR:-$HOME/actions-runner-loom-cloud}"
RUNNER_NAME="${RUNNER_NAME:-wh1-loom-cloud}"
RUNNER_LABELS="${RUNNER_LABELS:-self-hosted,linux,X64,wh1}"

if [[ "$EUID" -eq 0 ]]; then
  echo "✗ refuse to run as root. Switch to the runner user first."
  exit 1
fi

TOKEN=""
if [[ $# -ge 1 && -n "${1:-}" ]]; then
  TOKEN="$1"
elif [[ -n "${GITHUB_PAT:-}" ]]; then
  echo "→ fetching registration token via GITHUB_PAT..."
  TOKEN=$(curl -fsSL -X POST \
    -H "Accept: application/vnd.github+json" \
    -H "Authorization: Bearer ${GITHUB_PAT}" \
    -H "X-GitHub-Api-Version: 2022-11-28" \
    "https://api.github.com/repos/${REPO_OWNER}/${REPO_NAME}/actions/runners/registration-token" \
    | sed -n 's/.*"token": *"\([^"]*\)".*/\1/p')
  if [[ -z "$TOKEN" ]]; then
    echo "✗ failed to extract registration token from API response."
    echo "  Confirm the PAT has 'repo' (classic) or 'Administration: Read and Write' (fine-grained)."
    exit 1
  fi
else
  echo "✗ no auth provided."
  echo "  Either pass a registration token: $0 <reg-token>"
  echo "  Or export GITHUB_PAT and re-run."
  exit 1
fi

mkdir -p "$RUNNER_DIR"
cd "$RUNNER_DIR"

if [[ ! -f config.sh ]]; then
  ARCH=$(uname -m)
  case "$ARCH" in
    x86_64) RUNNER_ARCH="x64" ;;
    aarch64) RUNNER_ARCH="arm64" ;;
    *) echo "✗ unsupported arch $ARCH"; exit 1 ;;
  esac
  TARBALL="actions-runner-linux-${RUNNER_ARCH}-${RUNNER_VERSION}.tar.gz"
  URL="https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/${TARBALL}"
  echo "→ downloading $URL"
  curl -fsSLo "$TARBALL" "$URL"
  tar xzf "$TARBALL"
  rm "$TARBALL"
fi

echo "→ registering with $REPO_URL as $RUNNER_NAME"
./config.sh \
  --url "$REPO_URL" \
  --token "$TOKEN" \
  --name "$RUNNER_NAME" \
  --labels "$RUNNER_LABELS" \
  --unattended \
  --replace

echo ""
echo "✓ runner registered."
echo ""
echo "Next steps (need sudo):"
echo "  sudo $RUNNER_DIR/svc.sh install $(whoami)"
echo "  sudo $RUNNER_DIR/svc.sh start"
echo ""
echo "Verify on GitHub: https://github.com/${REPO_OWNER}/${REPO_NAME}/settings/actions/runners"
