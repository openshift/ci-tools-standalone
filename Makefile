TOOLS := backport-verifier ci-scheduling-webhook determinize-peribolos gpu-scheduling-webhook helpdesk-faq pipeline-controller pr-reminder publicize retester

.PHONY: build-all test clean $(addprefix build-,$(TOOLS)) $(addprefix image-,$(TOOLS))

build-all: $(addprefix build-,$(TOOLS))

build-%:
	go build -o _output/$* ./cmd/$*/

test:
	LANG=C LC_ALL=C go test ./...

clean:
	rm -rf _output

define image-rule
image-$(1): build-$(1)
	cp _output/$(1) images/$(1)/$(1)
	podman build -t $(1) images/$(1)/
	rm -f images/$(1)/$(1)
endef

$(foreach tool,$(TOOLS),$(eval $(call image-rule,$(tool))))
