#!/usr/bin/env bash
# 本地部署启停脚本（setsid 常驻，不依赖当前终端会话）
#
#   scripts/local-run.sh start     启动（端口被占用时拒绝重复启动）
#   scripts/local-run.sh stop      优雅停止（超时后强杀）
#   scripts/local-run.sh restart   重启
#   scripts/local-run.sh status    查看状态、健康检查与控制台资源形态
#   scripts/local-run.sh logs      跟踪日志
#
# 可通过环境变量覆盖：CONFIG / BIN / LISTEN
#
# 关于 BIN：默认是 bin/aigw，即 `make build` 的发布产物（控制台资源已混淆）。
# 想跑未混淆的调试版请显式指定 BIN=bin/aigw-src —— `make build-src` 只写这个文件，
# 不再覆盖 bin/aigw（M54；见 docs/design/m54-console-asset-shape.md）。
#
# 关于运行位置（重要）：请在普通终端里运行本脚本，不要在 DSH 的命令/后台任务里跑。
# DSH 每条命令都跑在独立的 `bwrap --unshare-pid --die-with-parent` 沙箱中，沙箱会回收
# 其中启动的进程（实测 setsid 也留不住，只是回收有几十秒延迟），所以从沙箱里启动的
# 服务无法常驻。在宿主终端里启动的实例不受此影响，可长期运行。
#
# 关于“存活判定”：同样是 PID namespace 隔离的原因，跨命令用 `kill -0 <pid>` / `ps`
# 判断进程是否存在会得到假阴性（PID 在别的 namespace 里不可见）。本脚本因此以
# “端口是否被监听”作为权威判据，pidfile 只作辅助。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONFIG="${CONFIG:-$ROOT/config.yaml}"
BIN="${BIN:-$ROOT/bin/aigw}"
LISTEN="${LISTEN:-:8088}"
PORT="${LISTEN##*:}"
PIDFILE="$ROOT/data/aigw-local.pid"
LOGFILE="$ROOT/data/aigw-local.log"
HEALTH_URL="http://127.0.0.1${LISTEN}/admin/ui/"

pid_alive() {
  [[ -f "$PIDFILE" ]] || return 1
  local pid
  pid="$(cat "$PIDFILE" 2>/dev/null || true)"
  [[ -n "$pid" ]] || return 1
  kill -0 "$pid" 2>/dev/null
}

# 端口是否已被监听：这是跨 PID namespace 唯一可靠的存活信号。
port_busy() {
  if command -v ss >/dev/null 2>&1; then
    [[ -n "$(ss -ltnH "sport = :$PORT" 2>/dev/null)" ]] && return 0
  fi
  if command -v curl >/dev/null 2>&1; then
    curl -s -m 2 -o /dev/null "$HEALTH_URL" 2>/dev/null && return 0
  fi
  return 1
}

health() {
  command -v curl >/dev/null 2>&1 || return 0
  local code
  code="$(curl -s -m 5 -o /dev/null -w '%{http_code}' "$HEALTH_URL" 2>/dev/null || echo 000)"
  echo "health: $HEALTH_URL -> HTTP $code"
}

# 控制台资源形态（M54）：由实例自己回答。构建日志（"ui: minified …" / "ui: source
# assets …"）只留在构建者当时的终端上，所以"8088 现在服务的是混淆版还是可读版"这个
# 问题以前只能靠 `strings bin/aigw | grep -c renderShell` 这种专家手法回答。
# 解析失败只影响这一行提示，绝不能让 status/start 失败。
console_shape() {
  command -v curl >/dev/null 2>&1 || { echo unknown; return 0; }
  local body
  body="$(curl -s -m 5 "http://127.0.0.1${LISTEN}/version" 2>/dev/null || true)"
  [[ -n "$body" ]] || { echo unknown; return 0; }
  if command -v python3 >/dev/null 2>&1; then
    printf '%s' "$body" | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    print("unknown"); raise SystemExit(0)
v = d.get("ui")
print(v if isinstance(v, str) and v else "unknown")' 2>/dev/null || echo unknown
  else
    printf '%s' "$body" | grep -o '"ui":"[a-z]*"' | cut -d'"' -f4 || echo unknown
  fi
}

report_console_shape() {
  local shape
  shape="$(console_shape)"
  case "$shape" in
    minified)
      echo "console: minified（控制台 js/css 已压缩混淆）"
      ;;
    source)
      echo "console: source（未混淆！）" >&2
      cat >&2 <<'EOF'
  这个实例控制台是**源码形态**：任何人拿到嵌入资源都能读到带完整中文注释的前端源码。
  只有调试控制台时才该这样跑（make build-src 现在写 bin/aigw-src，不会碰 bin/aigw）；
  要回到发布形态：
      make build && scripts/local-run.sh restart
EOF
      ;;
    *)
      echo "console: unknown（该二进制没有 ui 字段，早于 M54；不等于未混淆，请按需确认）"
      ;;
  esac
}

do_start() {
  if port_busy; then
    echo "端口 $PORT 已被占用，拒绝重复启动（否则新实例只会报 bind: address already in use 然后退出）" >&2
    echo "如需重启请先执行：$0 stop" >&2
    exit 1
  fi
  [[ -x "$BIN" ]] || { echo "binary not found or not executable: $BIN" >&2; exit 1; }
  [[ -f "$CONFIG" ]] || { echo "config not found: $CONFIG" >&2; exit 1; }
  mkdir -p "$ROOT/data"
  # cd 到仓库根目录：config.yaml 里的 ./data、./plugins 都是相对路径，必须有个确定基准。
  # setsid 让进程脱离当前会话与进程组；内层 bash 先写 pidfile 再 exec，
  # 保证记录下来的就是服务进程本身的 PID。
  cd "$ROOT"
  setsid bash -c 'echo $$ > "$1"; exec "$2" --config "$3"' _ \
    "$PIDFILE" "$BIN" "$CONFIG" >>"$LOGFILE" 2>&1 </dev/null &
  for _ in $(seq 1 50); do
    port_busy && break
    sleep 0.2
  done
  if port_busy && ! grep -q "http server failed" <(tail -n 3 "$LOGFILE" 2>/dev/null || true); then
    echo "started (pid $(cat "$PIDFILE" 2>/dev/null))  listen=$LISTEN"
    echo "log: $LOGFILE"
    health
    report_console_shape
  else
    echo "启动失败，见 $LOGFILE 末尾：" >&2
    tail -n 20 "$LOGFILE" >&2 || true
    exit 1
  fi
}

do_stop() {
  if pid_alive; then
    local pid
    pid="$(cat "$PIDFILE")"
    kill "$pid" 2>/dev/null || true
    for _ in $(seq 1 50); do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.2
    done
    if kill -0 "$pid" 2>/dev/null; then
      echo "优雅停止超时，强制结束 pid $pid"
      kill -9 "$pid" 2>/dev/null || true
    fi
    rm -f "$PIDFILE"
    echo "stopped"
    return 0
  fi
  if port_busy; then
    echo "端口 $PORT 仍有服务在监听，但 pidfile 里的进程在本命名空间不可见/已不存在。" >&2
    echo "该实例不是本 shell 启动的，无法从这里发信号。请在启动它的那个终端/宿主里执行：" >&2
    echo "  pkill -f 'bin/aigw --config'    # 或 kill <宿主 PID>" >&2
    exit 1
  fi
  rm -f "$PIDFILE"
  echo "not running"
}

do_status() {
  if pid_alive; then
    echo "running (pid $(cat "$PIDFILE"))  listen=$LISTEN"
  elif port_busy; then
    echo "running (端口 $PORT 被监听；守护进程 PID 在当前 PID namespace 不可见)"
  else
    echo "not running"
    return 0
  fi
  health
  report_console_shape
}

case "${1:-status}" in
  start)   do_start ;;
  stop)    do_stop ;;
  restart) do_stop; do_start ;;
  status)  do_status ;;
  logs)    tail -f "$LOGFILE" ;;
  *)       echo "usage: $0 {start|stop|restart|status|logs}" >&2; exit 2 ;;
esac
