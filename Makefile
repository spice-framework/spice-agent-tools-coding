.PHONY: tools-bootstrap fast check fmt benchmark verify verify-release

tools-bootstrap:
	go run ./internal/qualitygate -mode=tools-bootstrap

fast:
	go run ./internal/qualitygate -mode=fast

check:
	go run ./internal/qualitygate -mode=check

fmt:
	go run ./internal/qualitygate -mode=fmt

benchmark:
	go run ./internal/qualitygate -mode=benchmark

verify:
	go run ./internal/qualitygate -mode=verify

verify-release: verify
