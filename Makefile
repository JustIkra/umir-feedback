.PHONY: tools fmt fmt-all fmt-check build lint arch test check quality-test
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
