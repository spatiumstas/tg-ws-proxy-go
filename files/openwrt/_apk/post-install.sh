#!/bin/sh
[ "${IPKG_NO_SCRIPT}" = "1" ] && exit 0
[ -s ${IPKG_INSTROOT}/lib/functions.sh ] || exit 0
. ${IPKG_INSTROOT}/lib/functions.sh
export root="${IPKG_INSTROOT}"
export pkgname="tg-ws-proxy"
add_group_and_user

CONFIG_DIR="${IPKG_INSTROOT}/etc/tg-ws-proxy"
CONFIG_FILE="$CONFIG_DIR/config.conf"
SECRET_FILE="$CONFIG_DIR/secret.conf"

mkdir -p "$CONFIG_DIR"
[ -f "$CONFIG_FILE" ] || : > "$CONFIG_FILE"
[ -f "$SECRET_FILE" ] || printf 'SECRET=\n' > "$SECRET_FILE"

. "$SECRET_FILE" || true
if [ -z "${SECRET:-}" ]; then
	secret="$(${IPKG_INSTROOT}/usr/bin/tg-ws-proxy --gen-secret 2>/dev/null | tr -d ' \r\n' || true)"
	if [ "${#secret}" -eq 32 ]; then
		if grep -Eq '^[[:space:]]*SECRET=' "$SECRET_FILE"; then
			sed -i "s|^[[:space:]]*SECRET=.*$|SECRET=$secret|" "$SECRET_FILE"
		else
			printf 'SECRET=%s\n' "$secret" >> "$SECRET_FILE"
		fi
		echo "Generated SECRET in $SECRET_FILE"
	else
		echo "WARNING: failed to generate SECRET automatically" >&2
	fi
fi

default_postinst
