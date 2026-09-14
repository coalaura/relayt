#!/bin/bash

set -euo pipefail

if [ "${EUID}" -ne 0 ]; then
    echo "Run this script as root." >&2
    exit 1
fi

name="relayt"
path="/opt/relayt"
generated_sysusers="$(dirname "${BASH_SOURCE[0]}")/${name}.conf"
installed_sysusers="/etc/sysusers.d/${name}.conf"
owns_identity=false

if [ ! -L "${installed_sysusers}" ] && [ -f "${installed_sysusers}" ] && [ -f "${generated_sysusers}" ] && \
    cmp -s "${generated_sysusers}" "${installed_sysusers}"; then
    passwd_entry=$(getent passwd "${name}" || true)
    group_entry=$(getent group "${name}" || true)
    user_home=$(printf '%s' "${passwd_entry}" | cut -d: -f6)
    user_shell=$(printf '%s' "${passwd_entry}" | cut -d: -f7)

    if [ -n "${passwd_entry}" ] && [ -n "${group_entry}" ] && [ "${user_home}" = "${path}" ] && \
        { [ "${user_shell}" = "/sbin/nologin" ] || [ "${user_shell}" = "/usr/sbin/nologin" ]; }; then
        owns_identity=true
    fi
fi

echo "Stopping service..."
systemctl stop "${name}" 2>/dev/null || true

echo "Disabling service..."
systemctl disable "${name}" 2>/dev/null || true

echo "Removing unit file..."
rm -f "/etc/systemd/system/${name}.service"

echo "Removing sysusers config..."

if [ "${owns_identity}" = true ]; then
    rm -f "${installed_sysusers}"
else
    echo "Installed sysusers policy does not match; leaving it unchanged."
fi

if [ -f "/etc/logrotate.d/${name}" ]; then
    echo "Removing logrotate config..."
    rm -f "/etc/logrotate.d/${name}"
fi

echo "Reloading daemon..."
systemctl daemon-reload
systemctl reset-failed "${name}" 2>/dev/null || true

if [ "${owns_identity}" = true ]; then
    echo "Removing mksvc user and group..."

    if id "${name}" &>/dev/null; then
        userdel "${name}"
    fi

    if getent group "${name}" &>/dev/null; then
        groupdel "${name}"
    fi

else
    echo "Identity ownership could not be verified, leaving user and group unchanged."
fi

echo "Uninstall complete. Application files were left in place."
