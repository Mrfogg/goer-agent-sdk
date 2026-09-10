.PHONY: fmt fmt-check vet test test-race check

fmt:
	gofmt -w .

fmt-check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "unformatted files:"; echo "$$unformatted"; exit 1; \
	fi

vet:
	go vet ./...

test:
	go test ./...

test-race:
	go test -race ./...

check: fmt-check vet test
