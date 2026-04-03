# TG WS Proxy Go (KeeneticOS)

### Install

Repository:
```shell
curl -fsSL https://raw.githubusercontent.com/spatiumstas/feedly/main/add-repo.sh | sh
```
Package:
```shell
opkg install tg-ws-proxy
```

### Config

Main config file:

```shell
/opt/etc/tg-ws-proxy.conf
```

Minimal config example:

```conf
HOST=0.0.0.0
PORT=1443
SECRET=
LOG_LEVEL=0
DC_IP_DEFAULT=149.154.167.220
DC_IP_DEFAULT_POOL=""
EXTRA_ARGS=""
```

> Notes:

1. `SECRET` must be 32 hex chars. If empty, it is auto-generated during install.
2. `DC_IP_DEFAULT` and `DC_IP_DEFAULT_POOL` are global defaults for implicit DC map (`2,4`).
3. `EXTRA_ARGS` is for per-DC overrides and extra runtime flags.

Override examples:

```conf
# Per-DC pool override (DC2)
EXTRA_ARGS="--dc-ip-pool 2:149.154.175.50,149.154.167.220"

# Per-DC single IP override (DC203) + verbose logs
EXTRA_ARGS="--dc-ip 203:91.105.192.100 -v"
```

### Run

```shell
/opt/etc/init.d/S61tg-ws-proxy start
/opt/etc/init.d/S61tg-ws-proxy status
/opt/etc/init.d/S61tg-ws-proxy restart
/opt/etc/init.d/S61tg-ws-proxy stop
```

### Logs

If `LOG_LEVEL=1`, service logs are written to:

```shell
/opt/var/log/tg-ws-proxy.log
```

### Remove

```shell
opkg remove tg-ws-proxy
```

### Remove repository 
```shell
rm /opt/etc/opkg/feedly.conf
```