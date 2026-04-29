TOOLS := backport-verifier ci-scheduling-webhook determinize-peribolos gpu-scheduling-webhook helpdesk-faq pipeline-controller pr-reminder publicize retester

.PHONY: build-all test clean format gofmt lint validate-vendor $(addprefix build-,$(TOOLS)) $(addprefix image-,$(TOOLS))

build-all: $(addprefix build-,$(TOOLS))

build-%:
	go build -o _output/$* ./cmd/$*/

production-install:
	for tool in $(TOOLS); do \
		go install ./cmd/$$tool/; \
	done
.PHONY: production-install

test:
	LANG=C LC_ALL=C go test ./...
.PHONY: test

format: gofmt
.PHONY: format

gofmt:
	gofmt -s -w $(shell go list -f '{{ .Dir }}' ./... )
.PHONY: gofmt

lint:
	./hack/lint.sh
.PHONY: lint

validate-vendor:
	go mod tidy
	go mod vendor
	@if ! git diff --exit-code go.mod go.sum vendor/; then \
		echo "vendor is out of date, run 'go mod tidy && go mod vendor'"; exit 1; \
	fi
.PHONY: validate-vendor

clean:
	rm -rf _output

define image-rule
image-$(1): build-$(1)
	cp _output/$(1) images/$(1)/$(1)
	podman build -t $(1) images/$(1)/
	rm -f images/$(1)/$(1)
endef

$(foreach tool,$(TOOLS),$(eval $(call image-rule,$(tool))))
