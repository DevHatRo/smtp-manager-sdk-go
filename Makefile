.PHONY: test test-integration lint coverage clean

test:
	go test -race -coverprofile=coverage.out ./...

test-integration:
	go test -race -tags=integration -v ./...

lint:
	golangci-lint run ./...

coverage:
	go tool cover -html=coverage.out -o coverage.html

clean:
	rm -f coverage.out coverage.html
