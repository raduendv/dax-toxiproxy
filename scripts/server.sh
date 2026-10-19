#!/usr/bin/env bash
set -euo pipefail

command_name=${1:-}
shift || true
run_dir=${TOXIPROXY_RUN_DIR:-"${TMPDIR:-/tmp}/toxiproxy-${UID:-$(id -u)}"}
api_host=${TOXIPROXY_API_HOST:-127.0.0.1}
api_port=${TOXIPROXY_API_PORT:-8474}
api="http://$api_host:$api_port"
mkdir -p "$run_dir"

is_loopback() {
    [[ "$1" == localhost || "$1" == 127.* || "$1" == ::1 || "$1" == \[::1\] ]]
}
if ! is_loopback "$api_host" && [[ ${TOXIPROXY_ALLOW_REMOTE_API:-0} != 1 ]]; then
    printf 'Refusing non-loopback API bind without TOXIPROXY_ALLOW_REMOTE_API=1\n' >&2
    exit 2
fi

case "$command_name" in
    start)
        binary=${1:-${TOXIPROXY_BINARY:-toxiproxy-server}}
        command -v "$binary" >/dev/null 2>&1 || { printf 'Toxiproxy server binary not found: %s\n' "$binary" >&2; exit 1; }
        if curl --silent --fail --max-time 1 "$api/version" >/dev/null 2>&1; then
            printf 'Refusing to reuse an existing Toxiproxy API at %s\n' "$api" >&2
            exit 2
        fi
        pid_file="$run_dir/server.pid"
        if [[ -e "$pid_file" ]]; then
            read -r old_pid < "$pid_file"
            if kill -0 "$old_pid" 2>/dev/null; then
                printf 'Recorded Toxiproxy process is already alive (pid %s)\n' "$old_pid" >&2
                exit 2
            fi
            rm -f "$pid_file"
        fi
        nohup "$binary" -host="$api_host" -port="$api_port" -proxy-metrics > "$run_dir/server.log" 2>&1 < /dev/null &
        pid=$!
        printf '%s\n' "$pid" > "$pid_file"
        if curl --silent --show-error --fail --retry 10 --retry-connrefused --retry-delay 1 --max-time 15 "$api/version" > "$run_dir/version.json"; then
            printf 'Started Toxiproxy pid=%s api=%s\n' "$pid" "$api"
        else
            kill "$pid" 2>/dev/null || true
            rm -f "$pid_file"
            cat "$run_dir/server.log" >&2
            exit 1
        fi
        ;;
    status)
        curl --silent --show-error --fail --max-time 3 "$api/version"
        ;;
    stop)
        pid_file="$run_dir/server.pid"
        [[ -f "$pid_file" ]] || { printf 'No Toxiproxy PID owned by %s\n' "$run_dir"; exit 0; }
        read -r pid < "$pid_file"
        [[ "$pid" =~ ^[0-9]+$ ]] || { printf 'Invalid PID file\n' >&2; exit 1; }
        command_line=$(ps -p "$pid" -o command= 2>/dev/null || true)
        if [[ -z "$command_line" ]]; then
            rm -f "$pid_file"
            printf 'Toxiproxy already stopped\n'
            exit 0
        fi
        [[ "$command_line" == *toxiproxy-server* ]] || { printf 'PID %s is not Toxiproxy; refusing to signal\n' "$pid" >&2; exit 1; }
        kill "$pid"
        curl --silent --show-error --fail --retry 10 --retry-connrefused --retry-delay 1 --max-time 15 "$api/version" >/dev/null 2>&1 && { printf 'Toxiproxy did not stop after SIGTERM\n' >&2; exit 1; } || true
        rm -f "$pid_file"
        printf 'Stopped owned Toxiproxy pid=%s\n' "$pid"
        ;;
    *)
        printf 'Usage: %s start [BINARY] | status | stop\n' "$0" >&2
        exit 2
        ;;
esac
