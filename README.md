# TG WS Proxy Go for embedded devices ([FAQ](https://github.com/Flowseal/tg-ws-proxy/issues/389))

### Install

> KeeneticOS
Repository:
```shell
curl -fsSL https://raw.githubusercontent.com/spatiumstas/feedly/main/add-repo.sh | sh
```
Package:
```shell
opkg install tg-ws-proxy
```

> OpenWRT (IPK, APK)
Insert package link from Releases

```shell
opkg install %link%
```
APK
```shell
wget -O "/etc/apk/keys/tg-ws-proxy.pem" "https://github.com/spatiumstas/tg-ws-proxy-go/releases/download/0.4/tg-ws-proxy.pem"
apk add %link%
```

### Config

Main config file:

```shell
# Entware (KeeneticOS):
#   /opt/etc/tg-ws-proxy/config.conf
#   /opt/etc/tg-ws-proxy/secret.conf
# OpenWrt/generic opkg:
#   /etc/tg-ws-proxy/config.conf
#   /etc/tg-ws-proxy/secret.conf
```

Minimal config example:

```conf
# config.conf
HOST=0.0.0.0
PORT=1443
LOG_LEVEL=0
DC_IP_DEFAULT=149.154.167.220
DC_IP_DEFAULT_POOL=""
FAKE_TLS_DOMAIN=""
CFPROXY_FALLBACK_DOMAINS="pclead.co.uk,offshor.co.uk,cakeisalie.co.uk,noskomnadzor.co.uk,lovetrue.co.uk"
CFPROXY_DOMAINS_URL="https://raw.githubusercontent.com/Flowseal/tg-ws-proxy/main/.github/cfproxy-domains.txt"
EXTRA_ARGS=""

# secret.conf
SECRET=
```

> Notes:

1. `SECRET` must be 32 hex chars. If empty, it is auto-generated during install.
2. `DC_IP_DEFAULT` and `DC_IP_DEFAULT_POOL` are global defaults for implicit DC map (`2,4`).
3. `EXTRA_ARGS` is for per-DC overrides and extra runtime flags, [CFProxy](https://github.com/Flowseal/tg-ws-proxy/blob/main/docs/CfProxy.md)
4. Full list of available commands `--help`
5. `FAKE_TLS_DOMAIN` enables Fake TLS mode (`ee` secret link). Keep empty for standard `dd` mode.
6. `CFPROXY_FALLBACK_DOMAINS` - local fallback domain list.
7. `CFPROXY_DOMAINS_URL` [default value](https://raw.githubusercontent.com/Flowseal/tg-ws-proxy/main/.github/cfproxy-domains.txt)

Override examples:

```conf
# Per-DC pool override (DC2)
EXTRA_ARGS="--dc-ip-pool 2:149.154.175.50,149.154.167.220"

# Per-DC single IP override (DC203) + verbose logs
EXTRA_ARGS="--dc-ip 203:91.105.192.100 -v"

# Fake TLS mode (ee-secret)
FAKE_TLS_DOMAIN="example.com"
```

### Run

```shell
# Entware (KeeneticOS)
/opt/etc/init.d/S61tg-ws-proxy start
/opt/etc/init.d/S61tg-ws-proxy status
/opt/etc/init.d/S61tg-ws-proxy restart
/opt/etc/init.d/S61tg-ws-proxy stop

# OpenWrt/generic OPKG
service tg-ws-proxy start
service tg-ws-proxy status
service tg-ws-proxy restart
service tg-ws-proxy stop
```

### Logs

If `LOG_LEVEL=1`, service logs are written to:

```shell
# Entware (KeeneticOS): /opt/var/log/tg-ws-proxy.log
# OpenWrt/generic OPKG: /var/log/tg-ws-proxy.log
```

### Build from profile

```shell
cp config/entware/aarch64-3.10.config .config
make package
```

Output package:

```shell
.build/tg-ws-proxy_<version>-1_<platform>_<target>.ipk
```

### Remove

```shell
opkg remove tg-ws-proxy
```

### Remove repository

```shell
rm /opt/etc/opkg/feedly.conf
```
