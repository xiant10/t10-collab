# The targets every t10 sub project has (the workspace's proposals/dev-loop.md).
# CI runs make ci.
# make test needs nothing running; make test-db needs make db-up.

TEST_DATABASE_URL ?= postgres://postgres:test@localhost:55432/test?sslmode=disable
GO_FILES = $(shell git ls-files '*.go' | grep -v '^third_party/')

.PHONY: build lint test ci test-db db-up db-down

build:
	go build ./...

# gofmt comes from the toolchain go.mod pins, which go switches to; the gofmt on
# PATH may be another release, and they don't always agree.
lint:
	go vet ./...
	@out=$$("$$(go env GOROOT)/bin/gofmt" -l $(GO_FILES)); [ -z "$$out" ] || { echo "gofmt -w these:"; echo "$$out"; exit 1; }

# Database tests skip.
test:
	TEST_DATABASE_URL= go test ./...

# Each test rebuilds its schema, so packages run one at a time.
test-db:
	TEST_DATABASE_URL='$(TEST_DATABASE_URL)' go test -p 1 ./...

# The throwaway Postgres every t10 repo's tests share: started here, or reused
# when another repo started it.
db-up:
	@docker start t10-test-pg >/dev/null 2>&1 || docker run -d --name t10-test-pg -e POSTGRES_PASSWORD=test -e POSTGRES_DB=test -p 55432:5432 postgres:17-alpine >/dev/null
	@until docker exec t10-test-pg pg_isready -h 127.0.0.1 -U postgres -q; do sleep 1; done

db-down:
	@docker rm -f t10-test-pg >/dev/null

ci: lint build test-db
