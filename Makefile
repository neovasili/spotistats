# Spotistats — see docs/SPECS.md for the design and the milestone plan.
#
# Test layering (milestone 2):
#   test-short  pure tests only: no Docker, no network, no AWS. Must pass with Docker stopped.
#   test        adds the DynamoDB Local integration suite via testcontainers.
#               SPOTISTATS_TEST_REQUIRE_DDB=1 makes a missing/broken Docker a FAILURE rather
#               than a skip, so CI can never silently green out the whole integration suite.

GO         ?= go
PKGS       ?= ./...
COVERFILE  ?= coverage.out

.PHONY: all
all: lint test

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: fmt
fmt:
	$(GO) fmt $(PKGS)

.PHONY: vet
vet:
	$(GO) vet $(PKGS)

.PHONY: lint
lint: vet
	golangci-lint run

# No Docker, no network, no AWS. This is the target to run while iterating.
.PHONY: test-short
test-short:
	$(GO) test -short -race $(PKGS)

# Full suite. Requires a working Docker daemon for DynamoDB Local.
.PHONY: test
test:
	SPOTISTATS_TEST_REQUIRE_DDB=1 $(GO) test -race $(PKGS)

.PHONY: cover
cover:
	SPOTISTATS_TEST_REQUIRE_DDB=1 $(GO) test -race -coverprofile=$(COVERFILE) -covermode=atomic $(PKGS)
	$(GO) tool cover -func=$(COVERFILE) | tail -1
	@echo "html report: $(GO) tool cover -html=$(COVERFILE)"

# The aggregate engine is the correctness core; fuzz it before trusting a change.
.PHONY: fuzz
fuzz:
	$(GO) test -run '^$$' -fuzz FuzzAggregateDeltas -fuzztime 30s ./internal/model

.PHONY: clean
clean:
	rm -f $(COVERFILE) coverage.html
	$(GO) clean -testcache
