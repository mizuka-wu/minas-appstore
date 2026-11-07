.PHONY: build run-local check

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o dist/store .

run-local:
	go run . -catalog ./catalog/catalog.json -docker /bin/echo -assets ./dist-assets -uid 0 -nasip 127.0.0.1 -addr :8400 -dry-run

check:
	go vet ./... && gofmt -l . && go build ./...
