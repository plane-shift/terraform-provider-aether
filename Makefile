.PHONY: build test testacc generate docs openapi lint
build:      ; go build ./...
test:       ; go test ./... -count=1 -race
testacc:    ; TF_ACC=1 go test ./internal/acctest/... -count=1 -v -timeout 120m
generate:   ; go generate ./...
docs:       ; go run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs generate --provider-name aether
openapi:    ; ./scripts/fetch-openapi.sh && $(MAKE) generate
lint:       ; go vet ./... && test -z "$$(gofmt -l .)"
