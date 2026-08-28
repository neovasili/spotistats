# Spotistats — see docs/SPECS.md for the design and the milestone plan.
#
# Run `make help` for the target list.
#
# Two things worth knowing before using the deploy targets:
#
#   * `deploy` goes through CloudFormation and takes minutes. `push-lambdas` updates function
#     code directly and takes seconds. Use push-* while iterating on handler code and deploy
#     only when infrastructure actually changes.
#   * Everything under "local development" runs with no AWS account and no AWS credentials.
#     That includes the Spotify app credentials: put them in .dev/env (see `make dev-env`)
#     rather than SSM, otherwise the local targets fall back to Parameter Store and need AWS.

GO         ?= go
PKGS       ?= ./...
COVERFILE  ?= coverage.out

# Local secrets and overrides. .dev/ is gitignored, so this is where the Spotify client ID and
# secret live for local runs. Silently skipped when absent; run `make dev-env` to scaffold it.
DEV_ENV ?= .dev/env
-include $(DEV_ENV)

# Lambda functions, one per cmd/ directory. Adding a function means adding it here and
# nowhere else: every build, package and push target iterates over this list.
LAMBDAS    ?= capture query rollup enrich notify resolve

# Deployment identifiers. The stack names and the Lambda function names are pinned in infra/
# rather than generated, so they can be referenced directly without querying CloudFormation.
#
# Two stacks, two regions. The certificate must be in us-east-1 because that is the only region
# CloudFront accepts one from, and the billing budget is global; everything else lives in
# AWS_REGION. See docs/SPECS.md 3.1.
STACK        ?= SpotistatsStack
GLOBAL_STACK ?= SpotistatsGlobalStack
AWS_REGION   ?= eu-west-1
CERT_REGION  ?= us-east-1
FN_PREFIX    ?= spotistats

# Local development.
DDB_PORT      ?= 8000
DDB_ENDPOINT  ?= http://localhost:$(DDB_PORT)
DDB_CONTAINER ?= spotistats-ddb
DDB_IMAGE     ?= amazon/dynamodb-local:3.3.1
LOCAL_TABLE   ?= spotistats
# The deployed table (infra/config.go defaultTableName). Same name, different account.
PROD_TABLE    ?= spotistats
TOKEN_FILE    ?= ./.dev/refresh_token.json
# The unzipped GDPR "Extended Streaming History" export. Gitignored: it is personal data.
HISTORY_DIR   ?= ./.dev/historic-data

WEB_DIR    ?= web
BIN_DIR    ?= bin
LAMBDA_DIR ?= $(BIN_DIR)/lambda

# Exported so the CLI and the local API pick up the local backend without the caller having
# to remember six variables. Only affects targets in this file.
#
# PROD=1 points the CLI targets that can address either backend -- doctor, rollup, serve -- at
# the DEPLOYED table instead. Clearing the endpoint is what does it: internal/config treats an
# empty SPOTISTATS_DDB_ENDPOINT as "not local", so the SDK resolves the real regional endpoint.
#
# Local is the default deliberately, so no target can reach production by accident. The cost is
# the opposite trap: `make rollup` looks like it reconciles production and does not. Every
# affected target therefore prints which backend it is using before it runs.
#
# PROD=1 also clears the local token file and app credentials so the refresh token and secrets
# come from SSM, exactly as the Lambdas read them. Leaving SPOTISTATS_TOKEN_FILE set would
# point a production command at ./.dev/refresh_token.json and fail with "no refresh token
# stored" even though production has one.
ifeq ($(PROD),1)
export SPOTISTATS_DDB_ENDPOINT =
export SPOTISTATS_TABLE_NAME   = $(PROD_TABLE)
export SPOTISTATS_TOKEN_FILE   =
export SPOTISTATS_CLIENT_ID    =
export SPOTISTATS_CLIENT_SECRET =
export AWS_REGION
BACKEND = production ($(PROD_TABLE) in $(AWS_REGION), token + secrets from SSM)
else
export SPOTISTATS_DDB_ENDPOINT = $(DDB_ENDPOINT)
export SPOTISTATS_TABLE_NAME   = $(LOCAL_TABLE)
export SPOTISTATS_TOKEN_FILE   = $(TOKEN_FILE)
BACKEND = local ($(LOCAL_TABLE) at $(DDB_ENDPOINT)) -- pass PROD=1 for the deployed table
endif

# Exported from $(DEV_ENV) if it defined them. Without these the CLI falls back to reading the
# credentials from SSM, which needs AWS -- the one thing the local flow is meant to avoid.
# Under PROD=1 they are cleared above, so production reads SSM like the Lambdas do.
ifneq ($(PROD),1)
export SPOTISTATS_CLIENT_ID
export SPOTISTATS_CLIENT_SECRET
endif

# The Route 53 zone ID is an account-specific identifier, so it lives in $(DEV_ENV) rather than
# cdk.json (which holds only the domain and zone name). Exported unconditionally -- unlike the
# credentials above, a deploy is precisely when it is needed. Without it `cdk synth` fails the
# both-or-neither check in infra/config.go rather than dropping the alias records.
export SPOTISTATS_HOSTED_ZONE_ID

# MusicBrainz requires a descriptive User-Agent with contact information and throttles
# anonymous agents far harder as a class, so the client refuses to construct without this.
MUSICBRAINZ_CONTACT ?= https://github.com/neovasili/spotistats

# Extra flags for the CLI targets that accept them, e.g.
#   make resolve PROD=1 ARGS='-dry-run'
#   make rollup  PROD=1 ARGS='-all'
ARGS ?=
export SPOTISTATS_MUSICBRAINZ_CONTACT = $(MUSICBRAINZ_CONTACT)

.DEFAULT_GOAL := help

# ---------------------------------------------------------------------------
# help
# ---------------------------------------------------------------------------

.PHONY: help
help: ## Show this help
	@printf "Spotistats\n\nUsage: make <target>\n\n"
	@awk 'BEGIN {FS = ":.*?## "} \
		/^# ==== / { printf "\n\033[1m%s\033[0m\n", substr($$0, 8) } \
		/^[a-zA-Z0-9_%-]+:.*?## / { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 }' \
		$(MAKEFILE_LIST)
	@printf "\nLambdas: $(LAMBDAS)\n"

# ==== Quality

.PHONY: all
all: lint test ## Lint and run the full test suite

.PHONY: tidy
tidy: ## Tidy go.mod
	$(GO) mod tidy

.PHONY: fmt
fmt: ## Format all Go code
	$(GO) fmt $(PKGS)

.PHONY: vet
vet: ## Run go vet
	$(GO) vet $(PKGS)

.PHONY: lint
lint: vet ## Run go vet and golangci-lint
	golangci-lint run

.PHONY: test-short
test-short: ## Run pure tests only: no Docker, no network, no AWS
	$(GO) test -short -race $(PKGS)

.PHONY: test
test: ## Run the full suite, including DynamoDB Local integration tests
	SPOTISTATS_TEST_REQUIRE_DDB=1 $(GO) test -race $(PKGS)

.PHONY: cover
cover: ## Run the full suite with coverage
	SPOTISTATS_TEST_REQUIRE_DDB=1 $(GO) test -race -coverprofile=$(COVERFILE) -covermode=atomic $(PKGS)
	$(GO) tool cover -func=$(COVERFILE) | tail -1
	@echo "html report: $(GO) tool cover -html=$(COVERFILE)"

.PHONY: fuzz
fuzz: ## Fuzz the aggregate engine, the correctness core
	$(GO) test -run '^$$' -fuzz FuzzAggregateDeltas -fuzztime 30s ./internal/model

.PHONY: ci
ci: lint test-short test synth ## Everything CI runs

# ==== Build

.PHONY: build
build: build-cli build-lambdas ## Build every Go binary (CLI + all Lambdas)

.PHONY: build-cli
build-cli: ## Build the operator CLI to bin/spotistats
	$(GO) build -o $(BIN_DIR)/spotistats ./cmd/spotistats
	@echo "built $(BIN_DIR)/spotistats"

.PHONY: build-lambdas
build-lambdas: $(addprefix build-lambda-,$(LAMBDAS)) ## Build every Lambda binary

# provided.al2023 expects a statically linked executable named `bootstrap`. arm64 is cheaper
# per millisecond than x86_64, and lambda.norpc drops the unused RPC transport.
.PHONY: build-lambda-%
build-lambda-%: ## Build one Lambda binary (e.g. build-lambda-capture)
	@mkdir -p $(LAMBDA_DIR)/$*
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build \
		-tags lambda.norpc -ldflags="-s -w" \
		-o $(LAMBDA_DIR)/$*/bootstrap ./cmd/$*
	@echo "built $(LAMBDA_DIR)/$*/bootstrap"

.PHONY: package-lambdas
package-lambdas: $(addprefix package-lambda-,$(LAMBDAS)) ## Zip every Lambda for direct upload

# CDK zips directory assets itself; these zips are only for the push-* fast path.
.PHONY: package-lambda-%
package-lambda-%: build-lambda-% ## Zip one Lambda (e.g. package-lambda-capture)
	@cd $(LAMBDA_DIR)/$* && zip -q -X -r ../$*.zip bootstrap
	@echo "packaged $(LAMBDA_DIR)/$*.zip"

.PHONY: build-web
build-web: check-web ## Build the frontend production bundle
	cd $(WEB_DIR) && npm ci && npm run build
	@echo "built $(WEB_DIR)/dist"

# ==== Infrastructure

# Both regions need bootstrapping: CDK stages assets per environment, and the certificate
# stack lives in us-east-1.
.PHONY: bootstrap
bootstrap: check-aws ## CDK bootstrap; run once per account, for both regions
	@acct=$$(aws sts get-caller-identity --query Account --output text); \
	cdk bootstrap "aws://$$acct/$(AWS_REGION)" "aws://$$acct/$(CERT_REGION)"

.PHONY: synth
synth: build-lambdas ## Synthesise both CloudFormation templates (no credentials needed)
	cdk synth --quiet
	@echo "templates: cdk.out/$(GLOBAL_STACK).template.json cdk.out/$(STACK).template.json"

.PHONY: diff
diff: check-aws build-lambdas ## Show what a deploy would change, both stacks
	cdk diff

# --all deploys both stacks in dependency order: the certificate first, since CloudFront
# cannot reference it until it exists and has been validated.
#
# The FIRST deploy can take a while at the certificate step: ACM waits for the DNS validation
# record to propagate. With the hosted zone configured that record is created automatically.
.PHONY: deploy
deploy: check-aws build-lambdas ## Deploy both stacks via CloudFormation (minutes)
	cdk deploy --all --require-approval broadening

.PHONY: deploy-ci
deploy-ci: check-aws build-lambdas ## Deploy both stacks without prompting, for automation
	cdk deploy --all --require-approval never

.PHONY: outputs
outputs: check-aws ## Print the deployed stack outputs, both stacks
	@echo "$(GLOBAL_STACK) ($(CERT_REGION)):"
	@aws cloudformation describe-stacks --stack-name $(GLOBAL_STACK) --region $(CERT_REGION) \
		--query 'Stacks[0].Outputs[].[OutputKey,OutputValue]' --output table 2>/dev/null || echo "  not deployed"
	@echo "$(STACK) ($(AWS_REGION)):"
	@aws cloudformation describe-stacks --stack-name $(STACK) --region $(AWS_REGION) \
		--query 'Stacks[0].Outputs[].[OutputKey,OutputValue]' --output table 2>/dev/null || echo "  not deployed"

.PHONY: destroy
destroy: check-aws ## Destroy both stacks. The DynamoDB table is RETAINed and survives
	@printf 'This deletes the Lambdas, schedule, alarms, topic, bucket and distribution\n'
	@printf 'across %s (%s) and %s (%s).\n' \
		"$(STACK)" "$(AWS_REGION)" "$(GLOBAL_STACK)" "$(CERT_REGION)"
	@printf 'The DynamoDB table has DeletionPolicy=Retain, so listening history SURVIVES.\n'
	@printf 'Type the stack name to confirm: '
	@read -r ans && [ "$$ans" = "$(STACK)" ] || { echo "aborted"; exit 1; }
	cdk destroy --all --force

# ==== Deploy (fast paths, no CloudFormation)

.PHONY: push-lambdas
push-lambdas: $(addprefix push-lambda-,$(LAMBDAS)) ## Update every Lambda's code directly (seconds)

# Skips CloudFormation entirely. Correct only when the handler code changed and nothing in
# infra/ did; a configuration or permission change still needs `make deploy`.
.PHONY: push-lambda-%
push-lambda-%: check-aws package-lambda-% ## Update one Lambda's code (e.g. push-lambda-capture)
	@aws lambda update-function-code \
		--function-name $(FN_PREFIX)-$* \
		--zip-file fileb://$(LAMBDA_DIR)/$*.zip \
		--region $(AWS_REGION) \
		--query '{Function:FunctionName,LastModified:LastModified,CodeSize:CodeSize}' \
		--output table
	@aws lambda wait function-updated --function-name $(FN_PREFIX)-$* --region $(AWS_REGION)
	@echo "$(FN_PREFIX)-$* updated"
	@# CloudFront caches API responses for an hour (s-maxage=3600), so a query-Lambda change is
	@# invisible at the edge until it expires. That reads exactly like the new field not working:
	@# it cost a false bug report during the artwork work.
	@if [ "$*" = "query" ]; then 		dist=$$(aws cloudformation describe-stacks --stack-name $(STACK) --region $(AWS_REGION) 			--query 'Stacks[0].Outputs[?OutputKey==`DistributionId`].OutputValue' --output text); 		if [ -n "$$dist" ]; then 			aws cloudfront create-invalidation --distribution-id "$$dist" --paths '/api/*' 				--query 'Invalidation.Status' --output text >/dev/null; 			echo "invalidated /api/* at the edge"; 		fi; 	fi

.PHONY: url
url: check-aws ## Print the deployed site and API URLs
	@aws cloudformation describe-stacks --stack-name $(STACK) --region $(AWS_REGION) \
		--query "Stacks[0].Outputs[?OutputKey=='SiteUrl'||OutputKey=='ApiUrl'].[OutputKey,OutputValue]" \
		--output table

.PHONY: invoke-capture
invoke-capture: check-aws ## Invoke the capture Lambda once, now
	@aws lambda invoke --function-name $(FN_PREFIX)-capture --region $(AWS_REGION) \
		--cli-binary-format raw-in-base64-out --payload '{}' /dev/stdout | tail -2

.PHONY: logs-capture
logs-capture: check-aws ## Tail the capture Lambda logs
	aws logs tail /aws/lambda/$(FN_PREFIX)-capture --follow --region $(AWS_REGION)

# notify-test publishes a real message to the alarm topic, so the WHOLE chain is exercised:
# topic policy, subscription, Lambda permission, SSM read, and Slack itself. Nothing short of
# this proves delivery -- which is the entire lesson of the email subscription that sat in
# PendingConfirmation while three alarms fired.
#
# The payload is shaped like a CloudWatch alarm rather than being free text, so the message that
# arrives is the message a real alarm would produce.
.PHONY: notify-test
notify-test: check-aws ## Publish a fake alarm to the SNS topic and check it reaches Slack
	@topic=$$(aws cloudformation describe-stacks --stack-name $(STACK) --region $(AWS_REGION) \
		--query "Stacks[0].Outputs[?OutputKey=='AlarmTopicArn'].OutputValue" --output text); \
	if [ -z "$$topic" ] || [ "$$topic" = "None" ]; then \
		echo "no AlarmTopicArn output on $(STACK)"; exit 1; \
	fi; \
	printf '%s\n' "publishing to $$topic"; \
	aws sns publish --region $(AWS_REGION) --topic-arn "$$topic" \
		--subject 'ALARM: "spotistats-NotifyTest" in EU (Ireland)' \
		--message '{"AlarmName":"spotistats-NotifyTest","AlarmDescription":"A test message from make notify-test. Nothing is wrong.","NewStateValue":"ALARM","NewStateReason":"Published by hand to verify the Slack channel end to end.","StateChangeTime":"2026-01-01T00:00:00.000+0000","Region":"EU (Ireland)","AWSAccountId":"000000000000","AlarmArn":"arn:aws:cloudwatch:$(AWS_REGION):000000000000:alarm:spotistats-NotifyTest","Trigger":{"MetricName":"NotifyTest","Namespace":"Spotistats"}}' \
		--output text --query MessageId; \
	printf '\nPublished. Check the Slack channel.\n'
	@printf 'If nothing arrives: make logs-notify\n'

.PHONY: logs-notify
logs-notify: check-aws ## Tail the Slack notifier logs
	aws logs tail /aws/lambda/$(FN_PREFIX)-notify --follow --region $(AWS_REGION)

# deploy-web and deploy-data need the bucket and distribution from the stack outputs, which
# arrive with the CloudFront work in milestone 6.
# `--delete` removes anything in the bucket that is not in web/dist. The rendered snapshots live
# in the SAME bucket under data/, so they must be excluded or every web deploy would wipe them and
# the dashboard would 403 until the next nightly rollup.
.PHONY: deploy-web
deploy-web: check-aws build-web ## Sync the frontend bundle to S3 and invalidate CloudFront
	@bucket=$$(aws cloudformation describe-stacks --stack-name $(STACK) --region $(AWS_REGION) \
		--query "Stacks[0].Outputs[?OutputKey=='WebBucketName'].OutputValue" --output text 2>/dev/null); \
	dist=$$(aws cloudformation describe-stacks --stack-name $(STACK) --region $(AWS_REGION) \
		--query "Stacks[0].Outputs[?OutputKey=='DistributionId'].OutputValue" --output text 2>/dev/null); \
	if [ -z "$$bucket" ] || [ "$$bucket" = "None" ]; then \
		echo "no WebBucketName output on $(STACK): the S3/CloudFront resources arrive in milestone 6"; \
		exit 1; \
	fi; \
	aws s3 sync $(WEB_DIR)/dist "s3://$$bucket" --delete \
		--cache-control 'public,max-age=31536000,immutable' \
		--exclude index.html --exclude 'data/*'; \
	aws s3 cp $(WEB_DIR)/dist/index.html "s3://$$bucket/index.html" \
		--cache-control 'no-cache,must-revalidate'; \
	aws cloudfront create-invalidation --distribution-id "$$dist" --paths '/index.html' '/data/*'

# The rollup Lambda publishes snapshots itself on its nightly run; this is the manual path for
# forcing a refresh without waiting for 03:15.
.PHONY: deploy-data
deploy-data: check-aws ## Force a snapshot refresh by invoking the rollup Lambda
	@aws lambda invoke --function-name $(FN_PREFIX)-rollup --region $(AWS_REGION) \
		--cli-binary-format raw-in-base64-out --payload '{}' /dev/stdout | tail -2

.PHONY: logs-rollup
logs-rollup: check-aws ## Tail the rollup Lambda logs
	aws logs tail /aws/lambda/$(FN_PREFIX)-rollup --follow --region $(AWS_REGION)

# ==== Local development

.PHONY: dev-env
dev-env: ## Scaffold .dev/env for local Spotify credentials
	@if [ -f $(DEV_ENV) ]; then \
		echo "$(DEV_ENV) already exists, leaving it alone"; \
	else \
		mkdir -p $$(dirname $(DEV_ENV)); \
		printf '# Local-only Spotify app credentials. This directory is gitignored.\n' > $(DEV_ENV); \
		printf '# From https://developer.spotify.com/dashboard (see docs/PREREQUISITES.md step 2).\n' >> $(DEV_ENV); \
		printf 'SPOTISTATS_CLIENT_ID=\n' >> $(DEV_ENV); \
		printf 'SPOTISTATS_CLIENT_SECRET=\n' >> $(DEV_ENV); \
		printf '\n# Route 53 hosted zone ID for the site domain (docs/PREREQUISITES.md step 7).\n' >> $(DEV_ENV); \
		printf '# Only needed to deploy; `make synth` fails without it rather than dropping DNS.\n' >> $(DEV_ENV); \
		printf 'SPOTISTATS_HOSTED_ZONE_ID=\n' >> $(DEV_ENV); \
		chmod 600 $(DEV_ENV); \
		echo "created $(DEV_ENV) (mode 0600) - fill in the values"; \
	fi

.PHONY: dev-all
dev-all: dev dev-seed rollup ## Full local setup: container, table, synthetic data, snapshots
	@printf '\nEverything is ready. In two terminals:\n'
	@printf '  make serve\n'
	@printf '  make web-dev\n'

.PHONY: rollup
rollup: build-cli ## Reconcile, refresh leaderboards and render snapshots (PROD=1 for the deployed table)
	@printf 'backend: %s\n\n' "$(BACKEND)"
	@$(BIN_DIR)/spotistats rollup $(ARGS)

.PHONY: smoke
smoke: check-web ## Browser smoke suite against the local dev server (SMOKE_BASE_URL to retarget)
	@cd $(WEB_DIR) && npx playwright test

.PHONY: smoke-install
smoke-install: check-web ## One-off: download the browser the smoke suite drives
	@cd $(WEB_DIR) && npx playwright install chromium

.PHONY: backfill-scan
backfill-scan: build-cli ## Describe the history export without writing anything (no AWS needed)
	@$(BIN_DIR)/spotistats backfill -path $(HISTORY_DIR) -dry-run

.PHONY: backfill-enrich
backfill-enrich: build-cli ## Resolve artist/album IDs for the export (slow, resumable; PROD=1)
	@printf 'backend: %s\n\n' "$(BACKEND)"
	@$(BIN_DIR)/spotistats backfill -path $(HISTORY_DIR) -enrich-only -yes -timeout 3h

.PHONY: backfill
backfill: build-cli ## Import the history export (run backfill-enrich first; PROD=1)
	@printf 'backend: %s\n\n' "$(BACKEND)"
	@$(BIN_DIR)/spotistats backfill -path $(HISTORY_DIR) -timeout 4h

.PHONY: enrich
enrich: build-cli ## Backfill artist names/genres for artists already recorded (PROD=1 for the deployed table)
	@printf 'backend: %s\n\n' "$(BACKEND)"
	@$(BIN_DIR)/spotistats enrich

.PHONY: enrich-external
enrich-external: build-cli ## Resolve MusicBrainz + TheAudioDB artist facts (slow; PROD=1)
	@printf 'backend: %s\n\n' "$(BACKEND)"
	@$(BIN_DIR)/spotistats enrich-external -timeout 60m

.PHONY: resolve
resolve: build-cli ## Upgrade placeholder track rows to real Spotify IDs (budgeted; PROD=1)
	@printf 'backend: %s\n\n' "$(BACKEND)"
	@$(BIN_DIR)/spotistats resolve $(ARGS)

.PHONY: doctor
doctor: build-cli ## Diagnose unresolved leaderboard names (PROD=1 for the deployed table)
	@printf 'backend: %s\n\n' "$(BACKEND)"
	@$(BIN_DIR)/spotistats doctor

.PHONY: dev
dev: dev-up dev-table ## Start DynamoDB Local and create the table
	@printf '\nLocal backend ready.\n'
	@printf '  table:    %s at %s\n' "$(LOCAL_TABLE)" "$(DDB_ENDPOINT)"
	@printf '  token:    %s\n' "$(TOKEN_FILE)"
	@if [ -z "$$SPOTISTATS_CLIENT_ID" ]; then \
		printf '  creds:    NOT SET - run `make dev-env` and fill in %s,\n' "$(DEV_ENV)"; \
		printf '            otherwise the CLI falls back to SSM and needs AWS\n'; \
	else \
		printf '  creds:    from %s\n' "$(DEV_ENV)"; \
	fi
	@printf '\nNext:  make auth-login   then   make poll\n'

.PHONY: dev-up
dev-up: check-docker ## Start the DynamoDB Local container
	@if [ -n "$$(docker ps -q -f name=^$(DDB_CONTAINER)$$)" ]; then \
		echo "$(DDB_CONTAINER) already running"; \
	else \
		docker rm -f $(DDB_CONTAINER) >/dev/null 2>&1 || true; \
		docker run -d --name $(DDB_CONTAINER) -p $(DDB_PORT):8000 $(DDB_IMAGE) \
			-jar DynamoDBLocal.jar -inMemory -sharedDb >/dev/null; \
		echo "started $(DDB_CONTAINER) on port $(DDB_PORT)"; \
	fi
	@printf 'waiting for readiness'
	@for i in $$(seq 1 30); do \
		code=$$(curl -s -o /dev/null -w '%{http_code}' $(DDB_ENDPOINT) 2>/dev/null || echo 000); \
		if [ "$$code" = "400" ]; then printf ' ok\n'; exit 0; fi; \
		printf '.'; sleep 1; \
	done; printf '\nDynamoDB Local did not become ready\n'; exit 1

.PHONY: dev-down
dev-down: ## Stop and remove the DynamoDB Local container
	@docker rm -f $(DDB_CONTAINER) >/dev/null 2>&1 && echo "removed $(DDB_CONTAINER)" || echo "not running"

.PHONY: dev-table
dev-table: build-cli ## Create the table in DynamoDB Local
	@$(BIN_DIR)/spotistats init-table

.PHONY: dev-reset
dev-reset: dev-down dev-up dev-table ## Recreate the local backend from scratch (in-memory, so all data is lost)

.PHONY: dev-seed
dev-seed: build-cli ## Write synthetic listening data to the local table
	@$(BIN_DIR)/spotistats dev-seed

.PHONY: dev-config
dev-config: build-cli ## Show the resolved local configuration
	@$(BIN_DIR)/spotistats config

.PHONY: auth-login
auth-login: build-cli ## Run the one-time Spotify authorisation flow (opens a browser)
	@$(BIN_DIR)/spotistats auth login

.PHONY: auth-status
auth-status: build-cli ## Verify the stored refresh token still works
	@$(BIN_DIR)/spotistats auth status

.PHONY: poll
poll: build-cli ## Run one capture pass against the local backend
	@$(BIN_DIR)/spotistats poll

.PHONY: poll-dry
poll-dry: build-cli ## Show what a capture pass would ingest, writing nothing
	@$(BIN_DIR)/spotistats poll -dry-run

.PHONY: serve
serve: build-cli ## Run the query API for the frontend dev server (PROD=1 reads the deployed table)
	@printf 'backend: %s\n\n' "$(BACKEND)"
	@$(BIN_DIR)/spotistats serve

.PHONY: web-install
web-install: check-web ## Install frontend dependencies
	cd $(WEB_DIR) && npm install

# Mode A of docs/SPECS.md 7.4: point the dev server at any backend. There is no CORS
# anywhere in the system -- Vite proxies /api server-side, so the browser only sees localhost.
.PHONY: web-dev
web-dev: check-web ## Start the frontend dev server (VITE_API_TARGET selects the backend)
	cd $(WEB_DIR) && VITE_API_TARGET=$${VITE_API_TARGET:-http://127.0.0.1:8787} npm run dev

.PHONY: web-test
web-test: check-web ## Run the frontend tests
	cd $(WEB_DIR) && npm test

# ==== Utility

.PHONY: clean
clean: ## Remove build output, coverage and the synthesised template
	rm -rf $(BIN_DIR) cdk.out
	rm -f $(COVERFILE) coverage.html
	$(GO) clean -testcache

.PHONY: check-aws
check-aws: ## Verify AWS credentials resolve
	@aws sts get-caller-identity --query Account --output text >/dev/null 2>&1 || { \
		printf 'No usable AWS credentials.\n'; \
		printf '  export AWS_PROFILE=<profile>   (see: aws configure list-profiles)\n'; \
		printf '  aws sso login --profile <profile>\n'; \
		exit 1; }
	@printf 'aws: account %s, region %s\n' \
		"$$(aws sts get-caller-identity --query Account --output text)" "$(AWS_REGION)"

.PHONY: check-docker
check-docker: ## Verify a Docker daemon is reachable
	@docker info >/dev/null 2>&1 || { \
		printf 'No reachable Docker daemon. Start Docker Desktop, colima or Rancher Desktop.\n'; \
		exit 1; }

.PHONY: check-web
check-web:
	@test -f $(WEB_DIR)/package.json || { \
		printf 'No %s/package.json: the frontend arrives in milestone 7 (docs/SPECS.md 7).\n' "$(WEB_DIR)"; \
		exit 1; }
