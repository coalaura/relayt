#!/bin/bash

set -euo pipefail

if [ "${EUID}" -ne 0 ]; then
    echo "Run this script as root." >&2
    exit 1
fi

name="relayt"
path="/opt/relayt"
conf_dir="${path}/conf"
sysusers_file="/etc/sysusers.d/${name}.conf"

if [ -L "${path}" ] || [ ! -d "${path}" ]; then
    echo "Service path must be an existing, real directory: ${path}" >&2
    exit 1
fi

script_root=$(realpath "$(dirname "${BASH_SOURCE[0]}")/..")
service_root=$(realpath "${path}")

if [ "${script_root}" != "${service_root}" ]; then
    echo "Run the setup script from ${path}/conf." >&2
    exit 1
fi

for file in "${conf_dir}/${name}.conf" "${conf_dir}/${name}.service" "${conf_dir}/${name}_logs.conf"; do
    if [ -L "${file}" ] || [ ! -f "${file}" ]; then
        echo "Missing or unsafe generated file: ${file}" >&2
        exit 1
    fi
done

if [ -L "${path}/${name}" ] || [ ! -f "${path}/${name}" ]; then
    echo "Missing or unsafe service executable: ${path}/${name}" >&2
    exit 1
fi

echo "Stopping existing service..."

systemctl stop "${name}" 2>/dev/null || true

echo "Installing sysusers config..."

if [ -e "${sysusers_file}" ] || [ -L "${sysusers_file}" ]; then
    if [ -L "${sysusers_file}" ] || [ ! -f "${sysusers_file}" ] || ! cmp -s "${conf_dir}/${name}.conf" "${sysusers_file}"; then
        echo "Refusing to replace conflicting sysusers policy: ${sysusers_file}" >&2
        exit 1
    fi

    systemd-sysusers "${sysusers_file}"
else
    if getent passwd "${name}" >/dev/null || getent group "${name}" >/dev/null; then
        passwd_entry=$(getent passwd "${name}" || true)
        group_entry=$(getent group "${name}" || true)
        user_home=$(printf '%s' "${passwd_entry}" | cut -d: -f6)
        user_shell=$(printf '%s' "${passwd_entry}" | cut -d: -f7)

        if [ -z "${passwd_entry}" ] || [ -z "${group_entry}" ] || [ "${user_home}" != "${path}" ] || \
            { [ "${user_shell}" != "/sbin/nologin" ] && [ "${user_shell}" != "/usr/sbin/nologin" ]; }; then
            echo "Refusing to reuse existing user or group: ${name}" >&2
            exit 1
        fi

        echo "Adopting service identity created by an older release..."
    fi

    install -o root -g root -m 0644 "${conf_dir}/${name}.conf" "${sysusers_file}"
    systemd-sysusers "${sysusers_file}"
fi

passwd_entry=$(getent passwd "${name}" || true)
group_entry=$(getent group "${name}" || true)
user_home=$(printf '%s' "${passwd_entry}" | cut -d: -f6)
user_shell=$(printf '%s' "${passwd_entry}" | cut -d: -f7)

if [ -z "${passwd_entry}" ] || [ -z "${group_entry}" ] || [ "${user_home}" != "${path}" ] || \
    { [ "${user_shell}" != "/sbin/nologin" ] && [ "${user_shell}" != "/usr/sbin/nologin" ]; }; then
    echo "Service identity does not match generated policy: ${name}" >&2
    exit 1
fi

echo "Installing unit..."

install -o root -g root -m 0644 "${conf_dir}/${name}.service" "/etc/systemd/system/${name}.service"

if command -v logrotate >/dev/null 2>&1; then
    echo "Installing logrotate config..."

    install -o root -g root -m 0644 "${conf_dir}/${name}_logs.conf" "/etc/logrotate.d/${name}"
else
    echo "Logrotate not found, skipping..."
fi

echo "Setting permissions..."

chown root:root "${path}" "${conf_dir}" "${path}/${name}" "${conf_dir}/${name}.conf" "${conf_dir}/${name}.service" \
    "${conf_dir}/${name}_logs.conf" "${conf_dir}/setup.sh" "${conf_dir}/uninstall.sh" "${conf_dir}/svc.yml"
chmod 0755 "${path}"
chmod 0755 "${conf_dir}"
chmod 0755 "${path}/${name}"
chmod 0644 "${conf_dir}/${name}.conf" "${conf_dir}/${name}.service" "${conf_dir}/${name}_logs.conf"
chmod 0700 "${conf_dir}/setup.sh" "${conf_dir}/uninstall.sh" "${conf_dir}/svc.yml"

install -d -o "${name}" -g "${name}" -m 0750 "${path}/logs"
install -o "${name}" -g "${name}" -m 0640 /dev/null "${path}/logs/${name}.log"

install -d -o "${name}" -g "${name}" -m 0750 "${path}/data"

echo "Reloading daemon..."

systemctl daemon-reload
systemctl enable "${name}"

echo "Setup complete, starting service..."

systemctl restart "${name}"

echo "Done."
