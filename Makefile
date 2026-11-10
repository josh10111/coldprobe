# Needs: go, node/npm (after `cd web && npm ci`), docker, python3 and gitleaks.

.PHONY: test dev build up janitor worker-test scan

# -race needs cgo (gcc); the app itself builds with CGO_ENABLED=0.
test:
	go vet ./...
	go test -race ./...
	cd web && npm run build

# Go in fake mode on :8080, Vite on :5173 proxying /api to it. Ctrl-C stops both.
dev:
	go build -o bin/coldprobe ./cmd/coldprobe
	COLDPROBE_FAKE=1 ./bin/coldprobe serve & pid=$$!; \
	trap 'kill $$pid 2>/dev/null' INT TERM EXIT; \
	cd web && npm run dev

build:
	cd web && npm run build
	CGO_ENABLED=0 go build -trimpath -o bin/coldprobe ./cmd/coldprobe

up:
	docker compose up --build

# Lists leftover bench-coldprobe-* endpoints (dry run); delete with: make janitor ARGS=-delete
janitor:
	docker compose run --rm app janitor $(ARGS)

worker-test:
	cd worker && python3 -m venv .venv && .venv/bin/pip install -q -r requirements-dev.txt && .venv/bin/python -m pytest -q

# Whole history; the pre-commit hook scans only what's staged.
scan:
	gitleaks git --redact --no-banner
