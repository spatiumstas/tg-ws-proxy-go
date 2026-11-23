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
CFPROXY_WORKER_DOMAINS=""
EXTRA_ARGS=""

# secret.conf
SECRET=
```

> Примечания:

1. `SECRET` должен быть строкой из 32 hex-символов. Если оставить пустым, он будет автоматически сгенерирован при запуске.
2. `DC_IP_DEFAULT` и `DC_IP_DEFAULT_POOL` — глобальные значения по умолчанию для DC (`2,4`).
3. `EXTRA_ARGS` используется для переопределений по DC и дополнительных флагов, см. [CFProxy](https://github.com/Flowseal/tg-ws-proxy/blob/main/docs/RU/CfProxy.md).
4. Полный список доступных команд: `--help`.
5. `FAKE_TLS_DOMAIN` включает режим Fake TLS (`ee` secret link). Оставьте пустым для стандартного режима `dd`. Перед запуском приёма соединений прокси измеряет размер первой зашифрованной TLS-записи этого домена на порту 443 (таймаут 5 секунд). Если профиль недоступен, используются случайные размеры 64–160 байт.
6. `CFPROXY_DOMAINS` — локальный список fallback-доменов.
7. `CFPROXY_DOMAINS_URL` — [значение по умолчанию](https://raw.githubusercontent.com/Flowseal/tg-ws-proxy/main/.github/cfproxy-domains.txt)/[зеркало](https://raw.githubusercontent.com/spatiumstas/tg-ws-proxy-go/main-go/.github/cfproxy-domains.txt)
8. `CFPROXY_WORKER_DOMAINS` — домены своих Cloudflare Worker через запятую; в fallback они пробуются перед CF-прокси и TCP. [Настройка Worker](https://github.com/Flowseal/tg-ws-proxy/blob/main/docs/RU/CfWorker.md).
9. `--no-dc-ip DC` отключает прямой WS для указанного DC и переводит его на fallback. Флаг можно повторять; он имеет приоритет над `--dc-ip` и `--dc-ip-pool` независимо от порядка аргументов. Размер пула и прогрев Worker сохраняются.

10. При таймауте WSS CF-прокси и Worker автоматически пробуют `ws://` (HTTP, порт 80). Для каждого транспорта действует отдельная пауза, после которой WSS снова доступен для проверки. Ошибки сертификата и HTTP-ответы не вызывают переход на WS. При WS HTTP/WS-заголовки передаются без TLS-обёртки. `--no-secure` по-прежнему принудительно выбирает WS.
11. Worker с ответом HTTP 429 исключается до времени из `Retry-After`; если заголовок отсутствует или некорректен — до следующих суток UTC. Пауза общая для всех DC и прогрева пула.
12. Fake TLS запоминает принятые ClientHello на 241 секунду (до 65536 записей); повтор отправляется на домен маскировки.

Примеры переопределений:

```conf
# Переопределение пула для отдельного DC (DC2)
EXTRA_ARGS="--dc-ip-pool 2:149.154.175.50,149.154.167.220"

# Переопределение одним IP для отдельного DC (DC203) + логи
EXTRA_ARGS="--dc-ip 203:91.105.192.100 -v"

# Оставить прямой WS только для DC4 из стандартных DC2 и DC4
EXTRA_ARGS="--no-dc-ip 2"

# Отключить оба стандартных прямых маршрута и использовать fallback
EXTRA_ARGS="--no-dc-ip 2 --no-dc-ip 4"

# Свой Cloudflare Worker (домен без https:// и пути)
CFPROXY_WORKER_DOMAINS="random.username.workers.dev"

# Режим Fake TLS (ee-secret)
FAKE_TLS_DOMAIN="example.com"

# Отключить TLS для CF-прокси и CF-worker (ws://, порт 80)
EXTRA_ARGS="--no-secure"
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
