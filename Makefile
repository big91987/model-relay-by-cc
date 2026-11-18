.PHONY: build web test run verify-restart
build: web
	go build -o bin/gateway ./cmd/gateway
	go build -o bin/mockupstream ./cmd/mockupstream
web:
	cd web && npm install && npm run build
test:
	go test -race -count=1 ./...
run: build
	./bin/gateway
verify-restart: build
	scripts/verify_restart.sh
