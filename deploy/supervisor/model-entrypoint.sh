#!/bin/sh
set -eu

fail() { printf 'offline model: %s\n' "$*" >&2; exit 1; }
verify() {
    file=$1
    pin=$2
    case "$pin" in sha256:*) digest=${pin#sha256:} ;; *) fail "$3 must be sha256:<64 lowercase hex digits>" ;; esac
    case "$digest" in *[!0-9a-f]*|'') fail "$3 has an invalid SHA-256" ;; esac
    [ "${#digest}" -eq 64 ] || fail "$3 has an invalid SHA-256"
    [ -f "$file" ] && [ -r "$file" ] || fail "missing or unreadable file: $file"
    actual=$(sha256sum "$file") || fail "cannot hash $file"
    [ "${actual%% *}" = "$digest" ] || fail "SHA-256 mismatch: $file ($3)"
    printf 'offline model: verified %s\n' "$file"
}
# Only the verified files may be served. The role's arguments are an allowlist of the flags the
# compose profile uses (review of #298): anything else, such as an adapter, control vector,
# template file, second alias or download source, is refused before the server starts.
[ "$#" -ge 1 ] || fail 'expected chat or embed'
role=$1
shift
n=$#
i=0
while [ "$i" -lt "$n" ]; do
    arg=$1
    shift
    i=$((i + 1))
    case "$arg" in
        --host|--port|--ctx-size|--parallel|--chat-template-kwargs|-ngl|--threads)
            [ "$i" -lt "$n" ] || fail "missing value for $arg"
            set -- "$@" "$arg" "$1"
            shift
            i=$((i + 1)) ;;
        --jinja|--no-mmproj-offload)
            set -- "$@" "$arg" ;;
        *) fail "refusing argument not on the allowlist: $arg" ;;
    esac
done
if env | grep -Eq '^LLAMA_ARG_(HF|MODEL|MMPROJ|DOCKER|LORA|CONTROL_VECTOR|CHAT_TEMPLATE_FILE|ALIAS)'; then
    fail 'refusing LLAMA_ARG_* model, adapter or download variable'
fi
inside() {
    case "/$1/" in
        *'/../'*|'//'*) fail "model files must be inside the model directory: $1" ;;
    esac
}

# These are paths within the read-only model mount, including cache snapshot subdirectories.
models=${SUPERVISOR_MODELS_DIR:-/models}
case "$role" in
    chat)
        inside "${SUPERVISOR_AGENT_MODEL_FILE:-Qwen3.5-9B-Q4_K_M.gguf}"
        inside "${SUPERVISOR_AGENT_VISION_MODEL_FILE:-mmproj-BF16.gguf}"
        model=$models/${SUPERVISOR_AGENT_MODEL_FILE:-Qwen3.5-9B-Q4_K_M.gguf}
        projector=$models/${SUPERVISOR_AGENT_VISION_MODEL_FILE:-mmproj-BF16.gguf}
        verify "$model" "${SUPERVISOR_AGENT_MODEL_REVISION:-}" SUPERVISOR_AGENT_MODEL_REVISION
        verify "$projector" "${SUPERVISOR_AGENT_VISION_MODEL_REVISION:-}" SUPERVISOR_AGENT_VISION_MODEL_REVISION
        set -- -m "$model" --mmproj "$projector" --alias "${SUPERVISOR_AGENT_MODEL:-unsloth/Qwen3.5-9B-GGUF:Q4_K_M}" "$@"
        ;;
    embed)
        inside "${SUPERVISOR_EMBEDDINGS_MODEL_FILE:-Qwen3-Embedding-0.6B-Q8_0.gguf}"
        model=$models/${SUPERVISOR_EMBEDDINGS_MODEL_FILE:-Qwen3-Embedding-0.6B-Q8_0.gguf}
        verify "$model" "${SUPERVISOR_EMBEDDINGS_MODEL_REVISION:-}" SUPERVISOR_EMBEDDINGS_MODEL_REVISION
        set -- -m "$model" --alias "${SUPERVISOR_EMBEDDINGS_MODEL:-Qwen3-Embedding-0.6B}" --embeddings --pooling last "$@"
        ;;
    *) fail 'expected chat or embed' ;;
esac
PATH=$PATH:/app
export PATH
exec llama-server "$@"
