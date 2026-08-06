.PHONY: tools-bootstrap fast check fmt verify

tools-bootstrap:
	go run ./internal/qualitygate -mode=tools-bootstrap

fast:
	go run ./internal/qualitygate -mode=fast

check:
	go run ./internal/qualitygate -mode=check

fmt:
	go run ./internal/qualitygate -mode=fmt

verify:
	go run ./internal/qualitygate -mode=verify
