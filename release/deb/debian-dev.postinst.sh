#!/bin/sh
# Post-install for thundersnap-dev. Same as the main package but for
# thundersnapd-dev.service. Does NOT enable the service by default —
# the dev daemon should be started manually for testing.
if [ "$1" = "configure" ] || [ "$1" = "abort-upgrade" ] || [ "$1" = "abort-deconfigure" ] || [ "$1" = "abort-remove" ] ; then
	deb-systemd-helper unmask 'thundersnapd-dev.service' >/dev/null || true
	deb-systemd-helper update-state 'thundersnapd-dev.service' >/dev/null || true

	if [ -d /run/systemd/system ]; then
		systemctl --system daemon-reload >/dev/null || true
	fi

	# Create the dev state directory if it doesn't exist.
	# The main package's /var/lib/thundersnap is shared (data-dir),
	# but the dev daemon needs its own state-dir for tsnet identity.
	mkdir -p /var/lib/thundersnap-dev
	chmod 0700 /var/lib/thundersnap-dev

	echo "thundersnap-dev installed."
	echo "  Start with: systemctl start thundersnapd-dev"
	echo "  Config:     /etc/default/thundersnapd-dev"
	echo "  Policy:     /etc/thundersnap/policy-dev.jsonc"
	echo "  State:      /var/lib/thundersnap-dev (separate tsnet identity)"
	echo "  Data:       /var/lib/thundersnap (shared with main thundersnapd)"
fi
