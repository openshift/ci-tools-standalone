# ci-tools-standalone

Standalone CI tools extracted from [openshift/ci-tools](https://github.com/openshift/ci-tools). Each tool has minimal dependencies and runs independently.

## Tools

| Tool | Description |
|------|-------------|
| `backport-verifier` | Prow plugin that verifies backport PRs carry the correct labels and approvals |
| `ci-scheduling-webhook` | Kubernetes mutating admission webhook for CI workload scheduling and prioritization |
| `determinize-peribolos` | Deterministically formats Peribolos org configuration YAML |
| `gpu-scheduling-webhook` | Kubernetes mutating admission webhook for GPU/KVM workload scheduling |
| `helpdesk-faq` | Web service that serves helpdesk FAQ items from Kubernetes ConfigMaps |
| `pipeline-controller` | Kubernetes controller that manages CI pipeline resources |
| `pr-reminder` | Sends Slack reminders to team members about PRs awaiting review |
| `publicize` | Prow plugin that mirrors private PR merges to public repositories |
| `retester` | Periodically retests GitHub PRs based on configurable policies |

## Building

```bash
# Build all tools
make build-all

# Build a single tool
make build-backport-verifier

# Run tests
make test

# Build a container image
make image-publicize
```

## Repository layout

```
cmd/                    One subdirectory per tool
internal/gzip/          Shared gzip decompression utility (repo-private)
images/                 Dockerfiles per tool
```
