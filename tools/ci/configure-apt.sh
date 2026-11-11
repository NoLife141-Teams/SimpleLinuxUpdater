#!/usr/bin/env bash
set -euo pipefail
sudo tee /etc/apt/apt.conf.d/80-ci-network >/dev/null <<'EOF'
Acquire::Retries "5";
Acquire::http::Timeout "30";
Acquire::https::Timeout "30";
DPkg::Lock::Timeout "60";
EOF

# The Azure Ubuntu mirror serves release, updates and security pockets.
sudo sh "$(dirname "$0")/ssh-apt/configure-ubuntu-mirror.sh"
