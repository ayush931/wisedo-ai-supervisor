.PHONY: install format lint test check image clean web-dev infra-init infra-plan infra-apply train-dry-run train-pick data-prep

# Install Go modules and Web dependencies.
install:
	go mod download
	npm --prefix web ci --ignore-scripts

# Run frontend development server with hot reload and backend proxy.
web-dev:
	npm --prefix web run dev

# Format Go code.
format:
	gofmt -w cmd internal

# Static analysis and vetting.
lint:
	@test -z "$$(gofmt -l cmd internal)"
	go vet ./...

# Full Go unit test suite.
test:
	go test ./internal/... ./cmd/... ./deploy/...

# Complete local pre-commit gate.
check: supervisor-check

# Build the local production-like image.
image:
	docker build -t ghcr.io/wisedo-technology/wisedo-ai-supervisor:local .

clean:
	rm -rf dist build web/dist *.egg-info htmlcov .coverage

# --- AWS AI training infrastructure (WIS-234 / ADR 0004, offline only) ---
# Persistent platform lives in deploy/aws (Terraform); ephemeral training jobs
# are submitted by scripts/launch_training.py. Backend config is passed via
# TF_BACKEND_* so no state bucket is hardcoded in version control.
infra-init:
	terraform -chdir=deploy/aws init \
		-backend-config="bucket=$${TF_BACKEND_BUCKET:?set TF_BACKEND_BUCKET}" \
		-backend-config="key=$${TF_BACKEND_KEY:-oee-ai-training/staging.tfstate}" \
		-backend-config="region=$${AWS_DEFAULT_REGION:-ap-south-1}" \
		-backend-config="use_lockfile=true"

infra-plan:
	terraform -chdir=deploy/aws plan -out=tfplan

infra-apply:
	terraform -chdir=deploy/aws apply tfplan

# Preview the resolved SageMaker job request without submitting (reads .env).
# Uses system python3: the launcher is standalone (boto3 only needed to submit).
train-dry-run:
	python3 deploy/aws/scripts/launch_training.py --dry-run

# Guided picker for model, machine, spot, dataset (review-only, submits nothing).
train-pick:
	python3 deploy/aws/scripts/launch_training.py --interactive --dry-run

# Build an anonymized dataset version locally (RAW=file/dir/s3-uri, VERSION=tag).
# Uploads with --s3-upload s3://<bucket>/datasets/$(VERSION)/ when provided.
data-prep:
	@test -n "$(RAW)" || (echo "error: set RAW=<input path or s3 uri>" >&2; exit 1)
	python3 deploy/aws/training/preprocess.py \
		--input "$(RAW)" \
		--output-dir "$${OUT:-/tmp/oee-data}" \
		--dataset-version "$${VERSION:-v1.0.0}" \
		$(if $(S3_UPLOAD),--s3-upload $(S3_UPLOAD))
# Independent Go Supervisor foundation (WIS-290). No MES dependency.
.PHONY: supervisor-check supervisor-contracts supervisor-integration supervisor-e2e supervisor-build supervisor-migrate supervisor-eval supervisor-load supervisor-restore supervisor-release-check supervisor-demo-seed
supervisor-demo-seed:
	go run ./cmd/supervisor-demo-seed

supervisor-build:
	go build -trimpath -o build/supervisor ./cmd/supervisor
	npm --prefix web ci --ignore-scripts
	npm --prefix web run build

supervisor-check:
	@test -z "$$(gofmt -l cmd internal)"
	go vet ./...
	./scripts/supervisor/test.sh unit
	npm --prefix web ci --ignore-scripts
	npm --prefix web run build
	web/node_modules/.bin/tsc --noEmit -p web/personas/tsconfig.json
	node --test scripts/supervisor/facility-report.test.mjs
	npm --prefix web audit --audit-level=high

# Regenerates contracts/ (JSON Schemas and OpenAPI) and fails on any
# uncommitted drift, including new or stale files.
supervisor-contracts:
	go test -count=1 ./internal/config ./internal/httpapi ./internal/knowledge ./internal/contracts
	go run ./cmd/supervisor-contracts
	git diff --exit-code -- contracts
	@test -z "$$(git ls-files --others --exclude-standard contracts)" || { echo 'untracked contract artifacts: commit them' >&2; git ls-files --others --exclude-standard contracts >&2; exit 1; }

supervisor-migrate:
	go run ./cmd/supervisor migrate

supervisor-integration:
	./scripts/supervisor/test.sh integration

# E2E_ARGS passes Playwright filters. The @recent-auth case needs the API started with
# SUPERVISOR_SESSION_RECENT_AUTH=<n>s and SUPERVISOR_E2E_RECENT_AUTH_SECONDS=<n> (n <= 60); without
# them it reports BLOCKED. Other specs keep a session longer than 60s before a privileged action
# (walkthrough), so run two phases as CI does:
#   make supervisor-e2e E2E_ARGS='--grep-invert "@recent-auth|@agent-fixture|@agent-scripted|@onboarding-pilot|@feedback"'  (default window)
#   restart the API with SUPERVISOR_SESSION_RECENT_AUTH=20s, then
#   SUPERVISOR_E2E_RECENT_AUTH_SECONDS=20 make supervisor-e2e E2E_ARGS='--grep @recent-auth'
# Interview tags need SUPERVISOR_E2E_AGENT=off|fixture|scripted matching the API and worker mode
# (deploy/supervisor/README.md); a tagged test against the other mode is BLOCKED. CI runs the
# unscripted @agent-fixture refusal and scripted @agent-scripted draft in separate worker phases.
# CASE selects a named browser gate (rule 09: an unknown CASE fails, it never runs everything):
#   CASE=onboarding-pilot (WIS-631) runs @onboarding-pilot; it needs the API and worker started with
#   the pilot fixture script and chat answering, and SUPERVISOR_E2E_AGENT=pilot (README).
E2E_CASES := onboarding-pilot feedback
supervisor-e2e:
ifneq ($(CASE),)
	@case " $(E2E_CASES) " in *" $(CASE) "*) ;; *) echo "BLOCKED: unknown e2e CASE '$(CASE)' (known: $(E2E_CASES))" >&2; exit 2;; esac
	npm --prefix web run test:e2e -- --grep @$(CASE) $(E2E_ARGS)
else
	npm --prefix web run test:e2e -- $(E2E_ARGS)
endif

# WIS-270 knowledge-graph load qualification: thresholds frozen in
# deploy/qualification/knowledge-load.json; report in build/qualification/.
# REFERENCE=1 pins the client to GOMAXPROCS=4; the PostgreSQL container must be
# limited separately (docker update --cpus=4 --memory=8g <container>) and attested
# with SUPERVISOR_LOAD_PG_LIMITS=cpus=4,memory=8g, otherwise the report is labelled
# "not reference-host evidence". The onboarding gate (deploy/qualification/onboarding-load.json)
# ignores SUPERVISOR_LOAD_PG_LIMITS: it measures the host and cgroup limits itself.
# MANIFEST=deploy/qualification/chat-passages-load.json runs the WIS-626 chat passage probe at the
# 10,000-span bound and then its non-empty-selection companion, chat-passages-load-mixed.json.
supervisor-load:
	go build -trimpath -o build/supervisor-load ./cmd/supervisor-load
	$(if $(REFERENCE),GOMAXPROCS=4 )SUPERVISOR_LOAD_COMMAND='make supervisor-load$(if $(REFERENCE), REFERENCE=$(REFERENCE))$(if $(MANIFEST), MANIFEST=$(MANIFEST))' ./build/supervisor-load$(if $(MANIFEST), -manifest $(MANIFEST))

# WIS-280 eval harness (slice 1): MODE=deterministic | actual; a missing MODE, manifest, database
# or actual provider is BLOCKED (exit 2). Deterministic results never qualify.
# CASE=investigation,onboarding,chat narrows the suites (WIS-640; empty runs all, an unknown one is
# BLOCKED). MODE and CASE reach the shell only as environment variables, never spliced into the
# command text.
supervisor-eval: export SUPERVISOR_EVAL_MODE := $(MODE)
supervisor-eval: export SUPERVISOR_EVAL_CASE := $(CASE)
supervisor-eval:
	go build -trimpath -o build/supervisor-eval ./cmd/supervisor-eval
	SUPERVISOR_EVAL_COMMAND="make supervisor-eval MODE=$$SUPERVISOR_EVAL_MODE CASE=$$SUPERVISOR_EVAL_CASE" ./build/supervisor-eval -mode="$$SUPERVISOR_EVAL_MODE"

# Remaining product qualifications belong to subsequent implementation increments.
supervisor-restore supervisor-release-check:
	@echo 'BLOCKED: product qualification is not implemented by the WIS-290 foundation increment.' >&2
	@exit 2
