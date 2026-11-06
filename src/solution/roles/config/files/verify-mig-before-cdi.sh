#!/bin/sh
# Read-only guard. MIG is installed for boot by multi-user.target.wants.
set -eu

unit_link=/etc/systemd/system/multi-user.target.wants/gpu-mig-auto-enable.service
if [ -L "$unit_link" ] || [ -e "$unit_link" ]; then
    exec /usr/local/sbin/gpu-mig-auto-enable.sh verify
fi
