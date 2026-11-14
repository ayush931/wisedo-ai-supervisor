#!/bin/sh
# Behavioral startup regression; the optional argument permits a removed-check mutation control.
set -eu
entrypoint=${1:-$(dirname "$0")/model-entrypoint.sh}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
export SUPERVISOR_MODELS_DIR=$tmp
export SUPERVISOR_AGENT_MODEL_FILE=chat.gguf SUPERVISOR_AGENT_VISION_MODEL_FILE=vision.gguf SUPERVISOR_EMBEDDINGS_MODEL_FILE=embed.gguf
printf original > "$tmp/chat.gguf"
cp "$tmp/chat.gguf" "$tmp/vision.gguf"
cp "$tmp/chat.gguf" "$tmp/embed.gguf"
sum=$(sha256sum "$tmp/chat.gguf")
export SUPERVISOR_AGENT_MODEL_REVISION=sha256:${sum%% *}
export SUPERVISOR_AGENT_VISION_MODEL_REVISION=$SUPERVISOR_AGENT_MODEL_REVISION
export SUPERVISOR_EMBEDDINGS_MODEL_REVISION=$SUPERVISOR_AGENT_MODEL_REVISION
cat > "$tmp/llama-server" <<'EOF'
#!/bin/sh
printf 'SERVER_STARTED\n'
printf '%s\n' "$@"
EOF
chmod +x "$tmp/llama-server"
export PATH=$tmp:$PATH
# follows FLAG VALUE: VALUE is the argument right after FLAG.
follows() { awk -v f="$1" -v v="$2" 'p == f && $0 == v { ok = 1 } { p = $0 } END { exit !ok }' "$tmp/output"; }
# The default alias must be WIS-702's profile name (unset, so a changed default fails).
unset SUPERVISOR_AGENT_MODEL
# The compose default chat arguments, including --no-mmproj-offload.
sh "$entrypoint" chat --host 0.0.0.0 --port 8080 --ctx-size 16384 --parallel 1 --jinja \
    --chat-template-kwargs '{"enable_thinking":false}' -ngl 0 --no-mmproj-offload --threads 4 > "$tmp/output" 2>&1
grep -q '^SERVER_STARTED$' "$tmp/output"
follows -m "$tmp/chat.gguf" || { printf 'FAIL: chat model not passed with -m\n' >&2; exit 1; }
follows --mmproj "$tmp/vision.gguf" || { printf 'FAIL: projector not passed with --mmproj\n' >&2; exit 1; }
follows --alias unsloth/Qwen3.5-9B-GGUF:Q4_K_M || { printf 'FAIL: alias changed\n' >&2; exit 1; }
grep -q '^--no-mmproj-offload$' "$tmp/output"
sh "$entrypoint" embed --ctx-size 8192 > "$tmp/output" 2>&1
grep -q '^SERVER_STARTED$' "$tmp/output"
follows -m "$tmp/embed.gguf" || { printf 'FAIL: embedding model not passed with -m\n' >&2; exit 1; }
refuse() {
    if sh "$entrypoint" "$1" ${4:+"$4"} > "$tmp/output" 2>&1; then
        printf 'FAIL: %s was accepted\n' "$2" >&2; exit 1
    fi
    if grep -q '^SERVER_STARTED$' "$tmp/output"; then
        printf 'FAIL: server started before refusal\n' >&2; exit 1
    fi
    grep -q "$3" "$tmp/output"
    printf 'PASS: %s refused before serving\n' "$2"
}
for file in chat vision embed; do
    role=chat
    [ "$file" != embed ] || role=embed
    printf changed > "$tmp/$file.gguf"
    refuse "$role" "$file changed byte" 'SHA-256 mismatch'
    rm "$tmp/$file.gguf"
    refuse "$role" "$file missing file" 'missing or unreadable file'
    printf original > "$tmp/$file.gguf"
done
# Each case sets its variables in its own subshell, so nothing leaks into the next case
# (review of #298: "VAR=value refuse" keeps VAR set after the call when sh is bash).
( SUPERVISOR_AGENT_MODEL_REVISION=sha256:xyz; refuse chat 'malformed pin' 'invalid SHA-256' )
( SUPERVISOR_AGENT_MODEL_REVISION=; refuse chat 'missing chat pin' 'must be sha256:' )
( SUPERVISOR_AGENT_VISION_MODEL_REVISION=; refuse chat 'missing projector pin' 'must be sha256:' )
( SUPERVISOR_EMBEDDINGS_MODEL_REVISION=; refuse embed 'missing embedding pin' 'must be sha256:' )
for arg in -hf --hf-repo=unsloth/x -hff --model-url -mu -m --model=/models/other.gguf \
    --mmproj --mmproj-url -dr --docker-repo; do
    refuse chat "argument $arg" 'refusing' "$arg"
done
# Review of #298: only the arguments the compose profile uses are accepted. Adapters, control
# vectors and template files load unverified files; a second alias would rename the pinned model.
for arg in --lora --lora-scaled --control-vector --control-vector-scaled --control-vector-layer-range \
    --chat-template-file --alias -a --unknown-flag; do
    refuse chat "argument $arg" 'refusing' "$arg"
done
( LLAMA_ARG_HF_REPO=unsloth/x; export LLAMA_ARG_HF_REPO; refuse chat 'LLAMA_ARG_HF_REPO' 'refusing LLAMA_ARG_' )
( LLAMA_ARG_MODEL_URL=https://example.invalid/x.gguf; export LLAMA_ARG_MODEL_URL; refuse embed 'LLAMA_ARG_MODEL_URL' 'refusing LLAMA_ARG_' )
( LLAMA_ARG_LORA=/models/x.gguf; export LLAMA_ARG_LORA; refuse chat 'LLAMA_ARG_LORA' 'refusing LLAMA_ARG_' )
( LLAMA_ARG_CONTROL_VECTOR=/models/x.gguf; export LLAMA_ARG_CONTROL_VECTOR; refuse chat 'LLAMA_ARG_CONTROL_VECTOR' 'refusing LLAMA_ARG_' )
# A model file name must stay inside the read-only model directory.
( SUPERVISOR_AGENT_MODEL_FILE=../chat.gguf; refuse chat 'model file outside the model directory' 'inside the model directory' )
( SUPERVISOR_EMBEDDINGS_MODEL_FILE=/etc/passwd; refuse embed 'absolute model file' 'inside the model directory' )
refuse invalid 'unknown role' 'expected chat or embed'
printf 'PASS: valid chat/projector and embedding files start the server\n'
