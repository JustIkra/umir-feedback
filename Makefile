.PHONY: tools fmt fmt-all fmt-check build lint arch test check quality-test integration
tools:
	bash scripts/install-go-quality.sh
fmt:
	bash scripts/go-quality.sh fmt $(FILES)
fmt-all:
	bash scripts/go-quality.sh fmt-all
fmt-check build lint arch test check:
	bash scripts/go-quality.sh $@
quality-test:
	bash tests/quality/check.sh
integration:
	GOWORK=off go test -tags integration -p 1 -race -count=1 ./...
