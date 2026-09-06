# TG WS Proxy Go for embedded devices ([FAQ](https://github.com/Flowseal/tg-ws-proxy/issues/389))

### KeeneticOS

```shell
curl -fsSL https://raw.githubusercontent.com/spatiumstas/feedly/main/add-repo.sh | sh
opkg install tg-ws-proxy
```
### OpenWRT
Вставьте ссылку на пакет из Releases

> IPK

```shell
opkg install %link%
```
> APK
```shell
wget -O "/etc/apk/keys/tg-ws-proxy.pem" "https://github.com/spatiumstas/tg-ws-proxy-go/releases/latest/download/tg-ws-proxy.pem"
wget -O /tmp/tg-ws-proxy.apk %link%
apk add /tmp/tg-ws-proxy.apk
```

### Конфигурация

```shell
# Entware (KeeneticOS):
#   /opt/etc/tg-ws-proxy/config.conf
#   /opt/etc/tg-ws-proxy/secret.conf
# OpenWrt/generic opkg:
#   /etc/tg-ws-proxy/config.conf
#   /etc/tg-ws-proxy/secret.conf
```

```conf
# config.conf
HOST=0.0.0.0
PORT=1443
DC_IP_DEFAULT=149.154.167.220
DC_IP_DEFAULT_POOL=""
FAKE_TLS_DOMAIN=""
CFPROXY_DOMAINS=""
CFPROXY_DOMAINS_URL="https://raw.githubusercontent.com/Flowseal/tg-ws-proxy/main/.github/cfproxy-domains.txt"
EXTRA_ARGS=""

# secret.conf
SECRET=
```

> Примечания:

1. `SECRET` должен быть строкой из 32 hex-символов. Если оставить пустым, он будет автоматически сгенерирован при запуске.
2. `DC_IP_DEFAULT` и `DC_IP_DEFAULT_POOL` — глобальные значения по умолчанию для DC (`2,4`).
3. `EXTRA_ARGS` используется для переопределений по DC и дополнительных флагов, см. [CFProxy](https://github.com/Flowseal/tg-ws-proxy/blob/main/docs/CfProxy.md).
4. Полный список доступных команд: `--help`.
5. `FAKE_TLS_DOMAIN` включает режим Fake TLS (`ee` secret link). Оставьте пустым для стандартного режима `dd`.
6. `CFPROXY_DOMAINS` — локальный список fallback-доменов.
7. `CFPROXY_DOMAINS_URL` — [значение по умолчанию](https://raw.githubusercontent.com/Flowseal/tg-ws-proxy/main/.github/cfproxy-domains.txt)/[зеркало](https://raw.githubusercontent.com/spatiumstas/tg-ws-proxy-go/main-go/.github/cfproxy-domains.txt)

Примеры переопределений:

```conf
# Переопределение пула для отдельного DC (DC2)
EXTRA_ARGS="--dc-ip-pool 2:149.154.175.50,149.154.167.220"

# Переопределение одним IP для отдельного DC (DC203) + логи
EXTRA_ARGS="--dc-ip 203:91.105.192.100 -v"

# Режим Fake TLS (ee-secret)
FAKE_TLS_DOMAIN="example.com"
```

### Запуск

```shell
# Entware (KeeneticOS)
/opt/etc/init.d/S99tg-ws-proxy start
/opt/etc/init.d/S99tg-ws-proxy status
/opt/etc/init.d/S99tg-ws-proxy restart
/opt/etc/init.d/S99tg-ws-proxy stop

# OpenWrt/generic OPKG
service tg-ws-proxy start
service tg-ws-proxy status
service tg-ws-proxy restart
service tg-ws-proxy stop
```

### Запись логов

```conf
EXTRA_ARGS="-v"
```

```shell
# Entware (KeeneticOS): /opt/var/log/tg-ws-proxy.log
# OpenWrt/generic OPKG: /var/log/tg-ws-proxy.log
```

### Сборка

```shell
cp config/entware/aarch64-3.10.config .config
make package PKG_VERSION=1.2.3 PKG_REVISION=1
```


```shell
.build/tg-ws-proxy_<version>-1_<platform>_<target>.ipk
```

### Удаление пакета

```shell
opkg remove tg-ws-proxy
apk del tg-ws-proxy
```

### Удаление репозитория

```shell
rm /opt/etc/opkg/feedly.conf
rm /etc/apk/keys/tg-ws-proxy.pem
```
