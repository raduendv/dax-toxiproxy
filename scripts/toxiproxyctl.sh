#!/usr/bin/env bash
set -euo pipefail

api=${TOXIPROXY_API:-http://127.0.0.1:8474}
command_name=${1:-}
shift || true

if [[ "$api" =~ ^http://(localhost|127\.[0-9.]+|\[::1\]):[0-9]+$ ]]; then
    :
elif [[ ${TOXIPROXY_ALLOW_REMOTE_API:-0} == 1 && "$api" =~ ^https?://[^/@]+:[0-9]+$ ]]; then
    :
else
    printf 'Refusing unsafe Toxiproxy API URL: %s\n' "$api" >&2
    printf 'Use loopback, or explicitly opt in with TOXIPROXY_ALLOW_REMOTE_API=1.\n' >&2
    exit 2
fi

request() {
    method=$1
    endpoint=$2
    shift 2
    curl --silent --show-error --fail --max-time 10 -X "$method" "$api$endpoint" "$@"
}

case "$command_name" in
    list)
        [[ $# -eq 0 ]] || { printf 'list takes no arguments\n' >&2; exit 2; }
        request GET /proxies | jq .
        ;;
    get)
        [[ $# -eq 1 ]] || { printf 'get NAME\n' >&2; exit 2; }
        request GET "/proxies/$1" | jq .
        ;;
    create)
        [[ $# -eq 3 ]] || { printf 'create NAME LISTEN UPSTREAM\n' >&2; exit 2; }
        name=$1; listen=$2; upstream=$3
        [[ "$name" =~ ^[A-Za-z0-9_-]+$ && "$listen" =~ ^(127\.[0-9.]+|localhost|\[::1\]):[0-9]+$ && "$upstream" =~ ^[A-Za-z0-9.\[\]:_-]+:[0-9]+$ ]] || { printf 'Invalid name or address; listener must be loopback\n' >&2; exit 2; }
        jq -n --arg name "$name" --arg listen "$listen" --arg upstream "$upstream" '{name:$name,listen:$listen,upstream:$upstream,enabled:true}' | request POST /proxies -H 'Content-Type: application/json' --data-binary @- | jq .
        ;;
    delete)
        [[ $# -eq 1 && "$1" =~ ^[A-Za-z0-9_-]+$ ]] || { printf 'delete NAME\n' >&2; exit 2; }
        request DELETE "/proxies/$1"; printf '\n'
        ;;
    enable|disable)
        [[ $# -eq 1 && "$1" =~ ^[A-Za-z0-9_-]+$ ]] || { printf '%s NAME\n' "$command_name" >&2; exit 2; }
        enabled=true; [[ "$command_name" == enable ]] || enabled=false
        jq -n --argjson enabled "$enabled" '{enabled:$enabled}' | request POST "/proxies/$1" -H 'Content-Type: application/json' --data-binary @- | jq .
        ;;
    reset)
        [[ $# -eq 0 ]] || { printf 'reset takes no arguments\n' >&2; exit 2; }
        request POST /reset; printf '\n'
        ;;
    toxic-list)
        [[ $# -eq 1 && "$1" =~ ^[A-Za-z0-9_-]+$ ]] || { printf 'toxic-list NAME\n' >&2; exit 2; }
        request GET "/proxies/$1/toxics" | jq .
        ;;
    toxic-add)
        [[ $# -eq 4 && "$1" =~ ^[A-Za-z0-9_-]+$ && "$2" =~ ^[A-Za-z0-9_-]+$ && "$3" =~ ^(upstream|downstream)$ ]] || { printf 'toxic-add NAME TYPE STREAM ATTRIBUTES_JSON\n' >&2; exit 2; }
        name=$1; type=$2; stream=$3; attributes=$4
        printf '%s\n' "$attributes" | jq -e 'type == "object"' >/dev/null || { printf 'Attributes must be a JSON object\n' >&2; exit 2; }
        printf '%s\n' "$attributes" | jq -c --arg name "${type}_${stream}" --arg type "$type" --arg stream "$stream" '{name:$name,type:$type,stream:$stream,toxicity:1,attributes:.}' | request POST "/proxies/$name/toxics" -H 'Content-Type: application/json' --data-binary @- | jq .
        ;;
    toxic-remove)
        [[ $# -eq 2 && "$1" =~ ^[A-Za-z0-9_-]+$ && "$2" =~ ^[A-Za-z0-9_-]+$ ]] || { printf 'toxic-remove NAME TOXIC_NAME\n' >&2; exit 2; }
        request DELETE "/proxies/$1/toxics/$2"; printf '\n'
        ;;
    *)
        printf 'Commands: list, get, create, enable, disable, delete, reset, toxic-list, toxic-add, toxic-remove\n' >&2
        exit 2
        ;;
esac
