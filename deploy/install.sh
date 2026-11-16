#!/usr/bin/env bash
# Sets up (or re-applies) a ktx server: Caddy, brain-server, ktx-update,
# the SSH guard (public SSH closed while Tailscale is healthy) and automatic
# OS updates.
# Idempotent. Run as root from a checkout (or copy) of this repository:
#
#     sudo deploy/install.sh [path/to/brain-server path/to/caddy]
#
# Pass binaries only for the very first install; after that, ktx-update
# installs releases. Secrets are created on the server and never printed:
#   /etc/caddy/caddy.env           Cloudflare token (fill in by hand)
#   /etc/ktx/brain-server.env      API tokens are generated here
#   /etc/ktx/company-brain.key     deploy key for the data repo (copy in by hand)
#   /etc/ktx/authorized_keys       SSH public keys allowed into the web UI
set -euo pipefail
cd "$(dirname "$0")/.."

[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }

if command -v dnf >/dev/null; then
	dnf install -y -q git-core tar jq curl
elif command -v apt-get >/dev/null; then
	apt-get install -y -qq git tar jq curl
fi

id caddy >/dev/null 2>&1 || useradd --system --home-dir /var/lib/caddy --create-home --shell /sbin/nologin caddy
id ktx >/dev/null 2>&1 || useradd --system --home-dir /var/lib/ktx --no-create-home --shell /sbin/nologin ktx

install -d -m 755 /etc/caddy /etc/caddy/Caddyfile.d /opt/ktx/releases /usr/local/libexec
install -d -m 750 -o root -g ktx /etc/ktx

# Caddy
install -m 644 deploy/caddy/Caddyfile /etc/caddy/Caddyfile
[ -e /etc/caddy/Caddyfile.d/brain.caddyfile ] || install -m 644 deploy/caddy/brain.caddyfile /etc/caddy/Caddyfile.d/
[ -e /etc/caddy/caddy.env ] || install -m 640 -o root -g caddy deploy/caddy/caddy.env.example /etc/caddy/caddy.env

# brain-server
if [ ! -e /etc/ktx/brain-server.env ]; then
	tok() { openssl rand -hex 32; }
	sed -e "s/^BRAIN_API_TOKENS=.*/BRAIN_API_TOKENS=antonio:$(tok),maxim:$(tok)/" \
		deploy/brain-server.env.example >/etc/ktx/brain-server.env
	chown root:ktx /etc/ktx/brain-server.env
	chmod 640 /etc/ktx/brain-server.env
fi
# GitHub's SSH host keys, fetched over HTTPS from GitHub's API.
curl -fsSL https://api.github.com/meta | jq -r '.ssh_keys[] | "github.com " + .' >/etc/ktx/known_hosts.new
mv /etc/ktx/known_hosts.new /etc/ktx/known_hosts
chmod 644 /etc/ktx/known_hosts
for f in /etc/ktx/company-brain.key /etc/ktx/authorized_keys; do
	[ -e "$f" ] && chown ktx:ktx "$f" && chmod 600 "$f"
done

# Automatic OS updates: apply all updates daily, reboot when one needs it
# (kernel, glibc, ...). Everything below comes back up on boot.
if command -v dnf >/dev/null; then
	dnf install -y -q dnf-automatic
	sed -i -e 's/^apply_updates = .*/apply_updates = yes/' \
		-e 's/^reboot = .*/reboot = when-needed/' /etc/dnf/automatic.conf
	systemctl enable --now dnf-automatic.timer
elif command -v apt-get >/dev/null; then
	apt-get install -y -qq unattended-upgrades
	echo 'Unattended-Upgrade::Automatic-Reboot "true";' >/etc/apt/apt.conf.d/52ktx-reboot
	systemctl enable --now unattended-upgrades.service
fi

# Updater, SSH guard and units
install -m 755 deploy/ktx-update/ktx-update /usr/local/libexec/ktx-update
install -m 755 deploy/ssh-guard/ktx-ssh-guard /usr/local/libexec/ktx-ssh-guard
install -m 644 deploy/systemd/*.service deploy/systemd/*.path deploy/systemd/*.timer /etc/systemd/system/

# First install: use the binaries given on the command line.
if [ ! -e /opt/ktx/current ]; then
	if [ $# -ne 2 ]; then
		echo "first install: pass the brain-server and caddy binaries" >&2
		exit 1
	fi
	dest=/opt/ktx/releases/bootstrap
	install -d "$dest"
	install -m 755 "$1" "$dest/brain-server"
	install -m 755 "$2" "$dest/caddy"
	ln -sfn "$dest" /opt/ktx/current
fi

systemctl daemon-reload
systemctl enable --now caddy-reload.path ktx-update.timer ktx-ssh-guard.timer
systemctl enable tailscaled.service
systemctl enable caddy.service brain-server.service
systemctl restart brain-server.service
systemctl restart caddy.service # skipped until the Cloudflare token is set

echo
echo "brain-server: $(systemctl is-active brain-server.service)"
echo "caddy:        $(systemctl is-active caddy.service)"
echo "ssh guard:    $(cat /run/ktx-ssh-guard.state 2>/dev/null || echo 'starting')"
grep -q '^CLOUDFLARE_API_TOKEN=..*' /etc/caddy/caddy.env ||
	echo "Caddy waits for CLOUDFLARE_API_TOKEN in /etc/caddy/caddy.env; it starts by itself once that's saved."
