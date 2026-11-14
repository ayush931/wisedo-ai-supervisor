#!/usr/bin/env bash
# Local synthetic facility only; no deployment, hosted inference or destructive reset.
set -euo pipefail
umask 077
# Works from any checkout; the guards are the fixed project, ports and /var/tmp run root below.
repo=$(realpath -- "$(dirname -- "$0")/../..")
cd "$repo"
command -v go >/dev/null || { echo 'BLOCKED: Go toolchain not on PATH' >&2; exit 1; }
# Caches default to the root filesystem; callers may point them elsewhere.
export GOCACHE=${GOCACHE:-/var/tmp/gocache-factory} GOMODCACHE=${GOMODCACHE:-/var/tmp/gomod-factory} npm_config_cache=${npm_config_cache:-/var/tmp/npm-factory}
export GOTOOLCHAIN=local PLAYWRIGHT_BROWSERS_PATH=${PLAYWRIGHT_BROWSERS_PATH:-/var/tmp/playwright-factory}
export TMPDIR=/var/tmp GOMAXPROCS=4 GOFLAGS=-p=4
export COMPOSE_PROJECT_NAME=supervisor-facility FACILITY_REPO_ROOT="$repo" FACILITY_UID="$(id -u)" FACILITY_GID="$(id -g)"
export SUPERVISOR_ENVIRONMENT=local SUPERVISOR_PG_PORT=62477 SUPERVISOR_IDP_PORT=62077 SUPERVISOR_TEMPORAL_PORT=62277 SUPERVISOR_API_PORT=62376 SUPERVISOR_LOCAL_SUBNET=10.253.62.0/24
# Public local fixture credentials, never production credentials.
export POSTGRES_ADMIN_PASSWORD=facility-admin-fixture SUPERVISOR_MIGRATION_PASSWORD=facility-migrate-fixture SUPERVISOR_RUNTIME_PASSWORD=facility-runtime-fixture
export KEYCLOAK_DB_PASSWORD=facility-keycloak-fixture TEMPORAL_DB_PASSWORD=facility-temporal-fixture
export KEYCLOAK_ADMIN_USERNAME=local-admin KEYCLOAK_ADMIN_PASSWORD=facility-idp-fixture
export SUPERVISOR_LOCAL_ADMIN=local-admin SUPERVISOR_LOCAL_ADMIN_PASSWORD=$KEYCLOAK_ADMIN_PASSWORD SUPERVISOR_LOCAL_USER_PASSWORD=facility-user-fixture SUPERVISOR_OIDC_CLIENT_SECRET=facility-client-fixture
export SUPERVISOR_TEST_ADMIN_URL="postgresql://postgres:$POSTGRES_ADMIN_PASSWORD@127.0.0.1:62477/supervisor"
export SUPERVISOR_MIGRATION_URL="postgresql://supervisor_migrator:$SUPERVISOR_MIGRATION_PASSWORD@127.0.0.1:62477/supervisor"
export SUPERVISOR_DATABASE_URL="postgresql://supervisor_runtime:$SUPERVISOR_RUNTIME_PASSWORD@127.0.0.1:62477/supervisor"
export SUPERVISOR_TEST_DATABASE_URL=$SUPERVISOR_DATABASE_URL SUPERVISOR_PUBLIC_ORIGIN=http://127.0.0.1:62376 SUPERVISOR_LISTEN_ADDRESS=127.0.0.1:62376
export SUPERVISOR_LOCAL_KEYCLOAK=http://127.0.0.1:62077 SUPERVISOR_OIDC_ISSUER=http://127.0.0.1:62077/realms/supervisor-local
export SUPERVISOR_TEMPORAL_ADDRESS=127.0.0.1:62277 SUPERVISOR_TEMPORAL_TASK_QUEUE=supervisor-facility
root=/var/tmp/supervisor-facility
mkdir -p "$root/runs" "$root/bin"
command=${1:-help}
case "$command" in up|seed|auth|run|manuals|scenarios|personas|report|down|accept|config|local-model|audit|fixture|scenarios-fixture) ;; *) echo 'Usage: facility.sh up|seed|auth|run|manuals|scenarios|scenarios-fixture|personas|report|down|accept|config|local-model|audit|fixture'; exit 2;; esac
[[ $# -le 1 ]] || { echo 'Unexpected arguments' >&2; exit 2; }
available=$(df --output=avail -B1 /var/tmp | tail -1)
(( available > 5 * 1024 * 1024 * 1024 )) || { echo 'BLOCKED: disk' >&2; exit 1; }
owned=$(docker ps -aq --filter label=com.docker.compose.project=supervisor-facility)
if [[ $command == accept && -z $owned ]]; then
 "$repo/scripts/supervisor/facility.sh" up
 owned=$(docker ps -aq --filter label=com.docker.compose.project=supervisor-facility)
fi
if [[ $command == up ]]; then
 if [[ -z $owned ]]; then
  # Fresh data each time a stopped facility is brought up; prior runs remain intact.
  run_id=$(date -u +%Y%m%dT%H%M%SZ)
  mkdir "$root/runs/$run_id"
  printf '%s\n' "$run_id" > "$root/current.tmp"
  mv "$root/current.tmp" "$root/current"
 fi
fi
[[ -f $root/current && ! -L $root/current ]] || { echo 'BLOCKED: run facility.sh up first' >&2; exit 1; }
read -r run_id < "$root/current"
[[ $run_id =~ ^[0-9]{8}T[0-9]{6}Z$ ]] || { echo 'BLOCKED: invalid facility state' >&2; exit 1; }
export FACILITY_RUN_ROOT="$root/runs/$run_id"
[[ $(realpath "$FACILITY_RUN_ROOT") == "$FACILITY_RUN_ROOT" ]] || { echo 'BLOCKED: linked facility root' >&2; exit 1; }
artifacts="$FACILITY_RUN_ROOT/artifacts"
mkdir -p "$artifacts"
export FACILITY_SIM_BINARY="$root/bin/supervisor-factory-sim" FACILITY_RUN_REPORT="$artifacts/live/run-report.json"
# Raw session cookies exist only while a phase needs them; every exit, failed or not, removes them.
# `auth` alone keeps the file because writing it is that command's purpose.
sessions_file="$artifacts/auth/sessions.json"
live_pid=
cleanup() {
 if [[ -n $live_pid ]]; then kill -TERM "$live_pid" 2>/dev/null || true; wait "$live_pid" 2>/dev/null || true; fi
 [[ $command == auth ]] || rm -f -- "$sessions_file" "$artifacts/auth/.sessions.json"
}
trap cleanup EXIT
trap 'exit 130' INT TERM
model_lock="$root/model-request.lock"
compose=(docker compose -p supervisor-facility -f deploy/supervisor/compose.yml -f deploy/supervisor/facility.compose.yml)
for id in $owned; do
 [[ $(docker inspect --format '{{index .Config.Labels "com.wisedo.facility.root"}}' "$id") == "$FACILITY_RUN_ROOT" ]] || { echo 'BLOCKED: facility project belongs to another run; inspect it before proceeding' >&2; exit 1; }
done
wait_http() {
 local url=$1
 for ((i=0;i<90;i++)); do if curl --noproxy '*' --max-time 2 -fsS "$url" >/dev/null 2>&1; then return; fi; sleep 1; done
 echo "BLOCKED: facility service readiness timeout" >&2; return 1
}
check_config() {
 "${compose[@]}" config --format json | node -e '
 const fs=require("fs"),assert=require("assert/strict"),c=JSON.parse(fs.readFileSync(0,"utf8")),root=process.env.FACILITY_RUN_ROOT;
 assert.equal(c.name,"supervisor-facility");
 for(const [name,port]of [["postgres","62477"],["keycloak","62077"],["temporal","62277"]]){const p=c.services[name].ports;assert.equal(p.length,1);assert.equal(p[0].host_ip,"127.0.0.1");assert.equal(String(p[0].published),port);}
 const pg=c.services.postgres.volumes.find(v=>v.target==="/var/lib/postgresql/data");assert.equal(pg.type,"bind");assert.equal(pg.source,root+"/pgdata");
 for(const name of ["api","worker"]){assert.equal(c.services[name].user,process.env.FACILITY_UID+":"+process.env.FACILITY_GID);assert.equal(c.services[name].environment.SUPERVISOR_AGENT_PROVIDER,"fixture");assert.equal(c.services[name].environment.SUPERVISOR_TEMPORAL_TASK_QUEUE,"supervisor-facility");for(const v of c.services[name].volumes)assert.equal(v.read_only,true);}
 console.log("Facility compose scope and storage assertions passed.");'
}
auth() { FACILITY_ARTIFACTS="$artifacts/auth" web/node_modules/.bin/playwright test -c web/personas/playwright.config.ts auth.spec.ts; }
# runtime_env records the agent settings the running api and worker containers actually have
# (not the Compose file) and asserts the expected provider. Only allowlisted keys are copied.
runtime_env() {
 local expected=$1 out=$2 env_json='{}' name id
 for name in api worker; do
  id=$("${compose[@]}" ps -q "$name")
  [[ -n $id ]] || { echo "BLOCKED: facility $name is not running" >&2; return 1; }
  env_json=$(docker inspect --format '{{json .Config.Env}}' "$id" | node -e '
   const fs=require("fs"),[name,id,prev]=process.argv.slice(1),all=JSON.parse(fs.readFileSync(0,"utf8")),o=JSON.parse(prev),keep={};
   for(const kv of all){const i=kv.indexOf("="),k=kv.slice(0,i);if(/^SUPERVISOR_(AGENT_(PROVIDER|MODEL|MODEL_REVISION|BASE_URL|ENDPOINT_ADDRS|FIXTURE_SCRIPT|MAX_CONCURRENT|MAX_QUEUED|REASONING_EFFORT|TIMEOUT|MODE_ENABLED)|EMBEDDINGS_ENABLED)$/.test(k))keep[k]=kv.slice(i+1);}
   o[name]={container:id,environment:keep};console.log(JSON.stringify(o));' "$name" "$id" "$env_json")
 done
 printf '%s\n' "$env_json" | EXPECTED=$expected node -e '
  const fs=require("fs"),assert=require("assert/strict"),o=JSON.parse(fs.readFileSync(0,"utf8")),want=process.env.EXPECTED;
  for(const name of ["api","worker"]){const e=o[name].environment;assert.equal(e.SUPERVISOR_AGENT_PROVIDER,want,name+" provider");
   if(want==="local"){assert.equal(e.SUPERVISOR_AGENT_MODEL,"unsloth/Qwen3.5-9B-GGUF:Q4_K_M");assert.equal(e.SUPERVISOR_AGENT_MODEL_REVISION,"sha256:03b74727a860a56338e042c4420bb3f04b2fec5734175f4cb9fa853daf52b7e8");assert.equal(e.SUPERVISOR_AGENT_BASE_URL,"http://127.0.0.1:64601");assert.equal(e.SUPERVISOR_AGENT_ENDPOINT_ADDRS,"127.0.0.1:64601");assert.equal(e.SUPERVISOR_AGENT_MAX_CONCURRENT,"1");assert.equal(e.SUPERVISOR_AGENT_FIXTURE_SCRIPT,"");}}
  console.log("Running api and worker use the "+want+" provider.");' >&2
 printf '%s\n' "$env_json" > "$out"
}
wait_worker() {
 local worker_id ready=0
 worker_id=$("${compose[@]}" ps -q worker)
 for ((i=0;i<90;i++)); do
  [[ $(docker inspect --format '{{.State.Running}}' "$worker_id") == true ]] || { echo 'BLOCKED: facility worker exited' >&2; return 1; }
  if docker logs --tail 200 "$worker_id" 2>&1 | grep -F 'Started Worker' >/dev/null; then ready=1; break; fi
  sleep 1
 done
 ((ready)) || { echo 'BLOCKED: facility worker did not start' >&2; return 1; }
}
case "$command" in
 config) check_config;;
 up)
  if [[ -n $owned ]]; then check_config; echo "Facility already exists: $FACILITY_RUN_ROOT"; exit 0; fi
  for port in 62477 62077 62277 62376; do [[ -z $(ss -H -ltn "sport = :$port") ]] || { echo "BLOCKED: port $port is occupied" >&2; exit 1; }; done
  mkdir -p "$FACILITY_RUN_ROOT/pgdata" "$FACILITY_RUN_ROOT/image"
  cp deploy/supervisor/bootstrap.sh "$FACILITY_RUN_ROOT/bootstrap.sh"
  chmod 644 "$FACILITY_RUN_ROOT/bootstrap.sh"
  check_config
  for cmd in supervisor supervisor-factory-sim supervisor-seed supervisor-demo-seed; do CGO_ENABLED=0 go build -o "$root/bin/$cmd" "./cmd/$cmd"; done
  # What was built: commit, whether the tree was dirty, and the binary digests.
  commit=$(git rev-parse HEAD 2>/dev/null || echo unknown); dirty=unknown
  if [[ $commit != unknown ]]; then if [[ -z $(git status --porcelain) ]]; then dirty=false; else dirty=true; fi; fi
  printf '{"built_at":"%s","commit":"%s","working_tree_dirty":%s,"binaries":{%s}}\n' "$(date -u +%FT%TZ)" "$commit" "$([[ $dirty == unknown ]] && echo null || echo $dirty)" \
   "$(for cmd in supervisor supervisor-factory-sim; do printf '"%s":"%s",' "$cmd" "$(sha256sum "$root/bin/$cmd" | cut -d' ' -f1)"; done | sed 's/,$//')" > "$FACILITY_RUN_ROOT/build-provenance.json"
  npm --prefix web ci --ignore-scripts
  npm --prefix web run build
  node -e 'const {chromium}=require("./web/node_modules/playwright");chromium.launch({channel:"chromium",headless:true}).then(b=>b.close()).catch(()=>{console.error("BLOCKED: install Chromium with PLAYWRIGHT_BROWSERS_PATH=/var/tmp/playwright-factory");process.exit(1)});'
  cp "$root/bin/supervisor" "$FACILITY_RUN_ROOT/image/supervisor"
  cp /etc/ssl/certs/ca-certificates.crt "$FACILITY_RUN_ROOT/image/ca-certificates.crt"
  # Temporal reads its executable checksum; the non-root PDF child also re-execs it.
  chmod 0555 "$FACILITY_RUN_ROOT/image/supervisor"
  chmod 0444 "$FACILITY_RUN_ROOT/image/ca-certificates.crt"
  printf '%s\n' 'FROM scratch' 'COPY supervisor /supervisor' 'COPY ca-certificates.crt /etc/ssl/certs/ca-certificates.crt' 'ENTRYPOINT ["/supervisor"]' > "$FACILITY_RUN_ROOT/image/Dockerfile"
  docker build -t supervisor-facility:local "$FACILITY_RUN_ROOT/image"
  "${compose[@]}" up -d postgres keycloak temporal
  wait_http http://127.0.0.1:62077/realms/master/.well-known/openid-configuration
  ready=0
  for ((i=0;i<90;i++)); do if "${compose[@]}" exec -T temporal tctl --address temporal:7233 cluster health >/dev/null 2>&1; then ready=1; break; fi; sleep 1; done
  ((ready)) || { echo 'BLOCKED: Temporal readiness timeout' >&2; exit 1; }
  "$root/bin/supervisor" migrate
  echo "Infrastructure ready: $FACILITY_RUN_ROOT (run seed next)";;
 seed)
  [[ -n $owned ]] || { echo 'BLOCKED: no facility containers' >&2; exit 1; }
  "$root/bin/supervisor-seed"
  "$root/bin/supervisor-demo-seed"
  "${compose[@]}" up -d api worker
  wait_http http://127.0.0.1:62376/readyz
  wait_worker
  auth;;
 local-model)
  [[ -n $owned ]] || { echo 'BLOCKED: seed the facility first' >&2; exit 1; }
  check_config
  compose+=(-f deploy/supervisor/facility.local-model.yml)
  "${compose[@]}" config --format json | node -e '
   const fs=require("fs"),assert=require("assert/strict"),c=JSON.parse(fs.readFileSync(0,"utf8"));
   for(const name of ["api","worker"]){const e=c.services[name].environment;assert.equal(e.SUPERVISOR_AGENT_PROVIDER,"local");assert.equal(e.SUPERVISOR_AGENT_FIXTURE_SCRIPT,"");assert.equal(e.SUPERVISOR_AGENT_BASE_URL,"http://127.0.0.1:64601");assert.equal(e.SUPERVISOR_AGENT_ENDPOINT_ADDRS,"127.0.0.1:64601");assert.equal(e.SUPERVISOR_AGENT_MODEL,"unsloth/Qwen3.5-9B-GGUF:Q4_K_M");assert.equal(e.SUPERVISOR_AGENT_MODEL_REVISION,"sha256:03b74727a860a56338e042c4420bb3f04b2fec5734175f4cb9fa853daf52b7e8");assert.equal(e.SUPERVISOR_AGENT_MAX_CONCURRENT,"1");assert.equal(e.SUPERVISOR_EMBEDDINGS_ENABLED,"false");}
   console.log("Pinned local model and single-request assertions passed.");'
  "${compose[@]}" up -d --no-deps api worker
  wait_http http://127.0.0.1:62376/readyz
  wait_worker
  runtime_env local "$artifacts/local-model-runtime-env.json"
  # The endpoint must itself report the pinned model (read-only GET; no inference).
  endpoint=$(curl --noproxy '*' --max-time 5 -fsS http://127.0.0.1:64601/v1/models)
  printf '%s' "$endpoint" | node -e 'const d=JSON.parse(require("fs").readFileSync(0,"utf8")).data;if(d?.length!==1||d[0].id!=="unsloth/Qwen3.5-9B-GGUF:Q4_K_M"){console.error("BLOCKED: local endpoint does not report the pinned model");process.exit(1)}'
  printf '%s' "$endpoint" | BUILD="$FACILITY_RUN_ROOT/build-provenance.json" RUNTIME="$artifacts/local-model-runtime-env.json" node -e '
   const fs=require("fs"),d=JSON.parse(fs.readFileSync(0,"utf8")).data[0],read=f=>{try{return JSON.parse(fs.readFileSync(f,"utf8"))}catch{return null}};
   const out={schema_version:1,classification:"measured pinned-local-model synthetic plant; not qualification",started_at:new Date().toISOString(),build:read(process.env.BUILD),runtime:read(process.env.RUNTIME),endpoint_reported:{id:d.id,ftype:d.meta?.ftype,size_bytes:d.meta?.size,n_params:d.meta?.n_params},model_revision_source:"configured pin; the endpoint reports no weight digest",retrieval:"word; embeddings disabled"};
   fs.writeFileSync(process.env.FACILITY_RUN_ROOT+"/artifacts/local-model-provenance.json",JSON.stringify(out,null,2)+"\n",{mode:0o600});console.log("Local AC2/AC3 phase starts "+out.started_at);';;
 fixture)
  [[ -n $owned ]] || { echo 'BLOCKED: no facility containers' >&2; exit 1; }
  check_config
  "${compose[@]}" up -d --no-deps api worker
  wait_http http://127.0.0.1:62376/readyz
  wait_worker
  runtime_env fixture "$artifacts/fixture-runtime-env.json";;
 audit)
  FACILITY_ARTIFACTS="$artifacts" FACILITY_PG_CONTAINER="$("${compose[@]}" ps -q postgres)" node scripts/supervisor/facility-audit.mjs;;
 auth) auth;;
 run)
  [[ ! -e $artifacts/live ]] || { echo 'BLOCKED: live output already exists; down then accept for a fresh run' >&2; exit 1; }
  auth
  "$FACILITY_SIM_BINARY" run --url "$SUPERVISOR_PUBLIC_ORIGIN" --sessions "$artifacts/auth/sessions.json" --out "$artifacts/live" --speed 240 &
  live_pid=$!
  renewed=$SECONDS
  while kill -0 "$live_pid" 2>/dev/null; do
   if ((SECONDS-renewed >= 300)); then auth; renewed=$SECONDS; fi
   sleep 1
  done
  status=0; wait "$live_pid" || status=$?
  live_pid=
  exit "$status";;
 manuals)
  # AC3 runs only on the verified local model; anything else is refused before any request.
  runtime_env local "$artifacts/manuals-runtime-env.json"
  auth
  "$FACILITY_SIM_BINARY" manuals --url "$SUPERVISOR_PUBLIC_ORIGIN" --sessions "$artifacts/auth/sessions.json" --out "$artifacts/manuals";;
 scenarios)
  # revoked-document needs a first answer that cites its target; only the fixture provider gives
  # that deterministically, so it runs in scenarios-fixture (the report merges both).
  runtime_env local "$artifacts/scenarios-runtime-env.json"
  auth
  "$FACILITY_SIM_BINARY" scenarios --exclude revoked-document --url "$SUPERVISOR_PUBLIC_ORIGIN" --sessions "$artifacts/auth/sessions.json" --out "$artifacts/scenarios";;
 scenarios-fixture)
  runtime_env fixture "$artifacts/scenarios-fixture-runtime-env.json"
  auth
  "$FACILITY_SIM_BINARY" scenarios --scenario revoked-document --url "$SUPERVISOR_PUBLIC_ORIGIN" --sessions "$artifacts/auth/sessions.json" --out "$artifacts/scenarios-fixture";;
 personas)
  failed=0
  if FACILITY_ARTIFACTS="$artifacts/scripted" web/node_modules/.bin/playwright test -c web/personas/playwright.config.ts scripted.spec.ts; then :; else failed=1; fi
  if FACILITY_ARTIFACTS="$artifacts/exploratory" web/node_modules/.bin/playwright test -c web/personas/playwright.config.ts exploratory.spec.ts; then :; else failed=1; fi
  exit "$failed";;
 report)
  FACILITY_ARTIFACTS="$artifacts" node scripts/supervisor/facility-report.mjs;;
 down)
  check_config
  "${compose[@]}" down
  echo "Stopped only supervisor-facility; retained $FACILITY_RUN_ROOT";;
 accept)
  [[ ! -e $artifacts/live ]] || { echo 'BLOCKED: existing acceptance run; down first to retain it and start fresh' >&2; exit 1; }
  # New processes preserve errexit inside each phase, even when a negative gate returns nonzero.
  phase() { echo "FACILITY phase $1"; "$repo/scripts/supervisor/facility.sh" "$1" 2>&1 | tee "$artifacts/$1.log"; }
  "$repo/scripts/supervisor/facility.sh" up
  "$repo/scripts/supervisor/facility.sh" seed
  failed=0
  phase run || failed=1
  # AC3 and 19 of the 20 AC2 cases run on the pinned local model, holding the shared model lock for the whole
  # local phase. Any failure to select or verify it skips them; they never fall back to the fixture.
  exec 9>"$model_lock"
  if flock -n 9; then
   if phase local-model; then
    phase manuals || failed=1
    phase scenarios || failed=1
    phase audit || failed=1
   else failed=1; fi
   flock -u 9
  else echo 'BLOCKED: another facility model request holds the model lock' >&2; failed=1; fi
  exec 9>&-
  # Personas run last, on the fixture provider again, so their published claim cannot reach AC3.
  if phase fixture; then
   phase scenarios-fixture || failed=1
   phase personas || failed=1
  else failed=1; fi
  "$repo/scripts/supervisor/facility.sh" report || failed=1
  exit "$failed";;
esac
