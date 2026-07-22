#!/bin/sh
set -e
if [ -d /run/systemd/system ] ; then
	systemctl --system daemon-reload >/dev/null || true
fi

if [ -x "/usr/bin/deb-systemd-helper" ]; then
    if [ "$1" = "remove" ]; then
		deb-systemd-helper mask 'thundersnapd-dev.service' >/dev/null || true
	fi

    if [ "$1" = "purge" ]; then
		deb-systemd-helper purge 'thundersnapd-dev.service' >/dev/null || true
		deb-systemd-helper unmask 'thundersnapd-dev.service' >/dev/null || true
		# Remove dev state (tsnet identity). Data-dir is shared with the main
		# package and is NOT removed here.
		rm -rf /var/lib/thundersnap-dev
	fi
fi
