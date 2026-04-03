#!/bin/sh

PATH=/opt/sbin:/opt/bin:/usr/sbin:/usr/bin:/sbin:/bin

PIDFILE=/opt/var/run/tg-ws-proxy.pid
LOGFILE=/opt/var/log/tg-ws-proxy.log
BIN=/opt/bin/tg-ws-proxy
CONFFILE=/opt/etc/tg-ws-proxy.conf

if [ -f "$CONFFILE" ]; then
  . "$CONFFILE"
fi

get_pid() {
  [ -f "$PIDFILE" ] || return 1
  pid="$(cat "$PIDFILE" 2>/dev/null)"
  case "$pid" in
    ''|*[!0-9]*) return 1 ;;
  esac
  echo "$pid"
}

is_running() {
  pid_saved="$(get_pid)" || return 1
  if ! kill -0 "$pid_saved" 2>/dev/null; then
    return 1
  fi

  cmdline_file="/proc/$pid_saved/cmdline"
  [ -r "$cmdline_file" ] || return 1
  cmdline="$(tr '\000' ' ' < "$cmdline_file")"

  case "$cmdline" in
    *"$BIN"*) return 0 ;;
    *) return 1 ;;
  esac
}

cleanup_stale_pid() {
  if [ -f "$PIDFILE" ] && ! is_running; then
    rm -f "$PIDFILE"
  fi
}

load_runtime_args_from_pid() {
  pid="$(get_pid)" || return 1
  cmdline_file="/proc/$pid/cmdline"
  [ -r "$cmdline_file" ] || return 1

  rt_host=""
  rt_port=""
  rt_secret=""

  set -- $(tr '\000' ' ' < "$cmdline_file")
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --host)
        rt_host="$2"
        shift 2
        ;;
      --port)
        rt_port="$2"
        shift 2
        ;;
      --secret)
        rt_secret="$2"
        shift 2
        ;;
      *)
        shift
        ;;
    esac
  done

  RT_HOST="$rt_host"
  RT_PORT="$rt_port"
  RT_SECRET="$rt_secret"
  return 0
}

print_connect_link() {
  link_host="$HOST"
  link_port="$PORT"
  link_secret="$SECRET"

  if is_running && load_runtime_args_from_pid; then
    [ -n "$RT_HOST" ] && link_host="$RT_HOST"
    [ -n "$RT_PORT" ] && link_port="$RT_PORT"
    [ -n "$RT_SECRET" ] && link_secret="$RT_SECRET"
  fi

  if [ -z "$link_secret" ]; then
    echo "SECRET is empty in $CONFFILE" >&2
    return 1
  fi

  if [ "$link_host" = "0.0.0.0" ]; then
    br0_ip="$(ip -f inet addr show dev br0 2>/dev/null | sed -n 's/.*inet \([0-9.]\+\)\/.*/\1/p' | head -n 1)"
    [ -n "$br0_ip" ] && link_host="$br0_ip"
  fi

  echo "Connect link:"
  echo "  tg://proxy?server=$link_host&port=$link_port&secret=dd$link_secret"
}
