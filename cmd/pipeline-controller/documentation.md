# Pipeline Controller - User Guide

The Pipeline Controller is a tool that manages the execution of second-stage tests in pull requests. It automatically detects which tests should run based on file changes and repository configuration, helping to streamline the CI/CD process.

## Overview

The Pipeline Controller operates in three distinct modes, each offering different levels of automation for triggering second-stage tests. Second-stage tests are tests that run after the initial required tests pass, typically integration tests, optional tests, or tests that depend on specific file changes.

## Three Operating Modes

### 1. Manual Mode

In **Manual Mode**, the pipeline controller does not automatically trigger any tests. Users must manually trigger tests using commands.

**How it works:**
- When a PR is opened, the controller posts information in jobs status section which jobs belong to second stage.
- Users can trigger tests using:
  - `/test <job-name>` to run specific jobs
  - `/test ?` to see a list of all available jobs
  - `/pipeline required` to trigger all required and necessary jobs in second stage in this context

**When to use:**
- Teams that prefer full control over when tests run
- Repositories with expensive or time-consuming tests
- When you want to explicitly choose which tests to run

### 2. Automatic Mode

In **Automatic Mode**, the pipeline controller automatically triggers second-stage tests after all required tests pass successfully. This mode reassembles classical Jenkins or Bamboo pipeline expirience. 

**How it works:**
- When a PR is opened, the controller posts an informational comment
- Once all required tests (first stage) pass, the controller automatically:
  - Detects which tests should run based on file changes
  - Uses `pipeline_run_if_changed` and `pipeline_skip_if_only_changed` annotations to determine relevance
  - Triggers the appropriate second-stage tests automatically
- The controller posts a comment showing which tests will be scheduled

**When to use:**
- Teams that want fully automated test execution
- When you want to ensure all relevant tests run automatically

**Important Note:** If you manually trigger some second-stage tests before the automatic trigger occurs, the controller **complements** them rather than stepping aside. When the first stage completes it schedules only the remaining required jobs that do not yet have a run at the current HEAD (a "delta"), skipping the ones you already triggered. One manual trigger no longer moves the whole pipeline into manual control. To re-run a specific job that already ran, use `/test <job>`.

### 3. LGTM Mode

In **LGTM Mode**, the pipeline controller triggers second-stage tests when the `lgtm` label is added to a pull request.

**How it works:**
- When a PR is opened, the controller posts an informational comment
- When the `lgtm` label is added to the PR:
  - The controller automatically detects which tests should run based on file changes
  - Triggers the appropriate second-stage tests
  - Posts a comment showing which tests will be scheduled

**When to use:**
- Teams that want tests to run only when code review is approved
- To reduce unnecessary test runs during active development

**Important Note:** If you manually trigger some second-stage tests before the `lgtm` label is added, the controller **complements** them when the label lands: it schedules only the remaining required jobs that do not yet have a run at the current HEAD (a "delta"), skipping the ones you already triggered. One manual trigger no longer moves the whole pipeline into manual control. To re-run a specific job that already ran, use `/test <job>`.

## The `/pipeline required` Command

The `/pipeline required` command works in **all three modes** and allows you to explicitly request that the pipeline controller trigger all required and necessary second-stage tests.

**When to use `/pipeline required`:**
- In Manual Mode: To trigger all required tests at once
- In Automatic Mode: To trigger tests before they would automatically run, or to retrigger after manual intervention
- In LGTM Mode: To trigger tests before the LGTM label is added, or to retrigger after manual intervention
- When you want to force-run the entire required set at once (note: this re-fires jobs that are already running; to run only the jobs still missing at HEAD, use `/pipeline remaining`)

**Important:** `/pipeline required` force-schedules the whole required set, including jobs already running at the current HEAD. Using it in Automatic or LGTM mode does not disable automatic scheduling: the controller keeps auto-completing (delta) afterward — once these jobs exist at HEAD the next automatic pass simply computes an empty delta and schedules nothing new until the PR HEAD changes.

## The `/pipeline remaining` Command

The `/pipeline remaining` command works in **all three modes** and schedules the required second-stage tests **minus** the jobs that already have a run at the current HEAD — the same "delta" the Automatic and LGTM modes compute automatically, but on demand.

**How it differs from `/pipeline required`:**
- `/pipeline required` force-schedules the **entire** required set, re-firing jobs that are already running and spending CI twice.
- `/pipeline remaining` schedules **only the jobs that are still missing** at HEAD, leaving already-triggered jobs alone.

**When to use `/pipeline remaining`:**
- In Manual Mode, to "run the rest" after you triggered a few jobs yourself, without the force-all duplication of `/pipeline required`.
- In Automatic or LGTM mode, to fill gaps early — before the first stage completes — without re-running jobs that already started.

**Important:** Like `/pipeline required`, `/pipeline remaining` **bypasses the first-stage gate**. It can schedule second-stage jobs before the first-stage (always-required) tests pass. This is intentional for the "fill gaps early" use case, but it means the command does not wait for the first stage the way Automatic mode does. To force a re-run of a specific job that already ran, use `/test <job>`.

## Test Detection

The pipeline controller automatically detects which tests should run based on:

1. **Always required tests**: Tests that must always run
2. **Conditionally required tests**: Tests that run based on file changes using:
   - `pipeline_run_if_changed`: Tests run if matching files changed
   - `pipeline_skip_if_only_changed`: Tests skip if only matching files changed

The controller analyzes the files changed in your PR and determines which tests are relevant.

## Always Required Second-Stage Tests

Always required second-stage tests are tests that:
- Have `always_run: false` (they don't run automatically in the first stage)
- Are **not** marked as `optional: true` (they are required to pass)
- Do not have conditional annotations (`pipeline_run_if_changed` or `pipeline_skip_if_only_changed`)

These tests will always be triggered by the pipeline controller in the second stage, regardless of file changes. They represent essential tests that must pass before a PR can be merged, but are expensive enough to run only after the first-stage tests pass.

**Example configuration:**
```yaml
- always_run: false
  as: e2e-critical-test
  steps:
    workflow: openshift-e2e-test
```

Note: If a test is marked as `optional: true`, it will not be considered an always required test, even if it has `always_run: false`.

## Conditional Test Annotations

The pipeline controller supports two conditional annotations that allow tests to run based on file changes in the pull request:

### `pipeline_run_if_changed`

This annotation specifies that a test should run **only if** files matching the pattern have changed in the PR.

**How it works:**
- If any file in the PR matches the regex pattern, the test will be triggered
- If no files match the pattern, the test will be skipped
- Takes precedence over `pipeline_skip_if_only_changed` if both are present

**How to add it:**

Add the annotation to your test configuration in the ci-operator config file:

```yaml
- always_run: false
  as: e2e-builds-test
  annotations:
    pipeline_run_if_changed: ^(pkg/build)|^(test/extended/builds)
  steps:
    workflow: openshift-e2e-builds
```

**Pattern format:**
- Uses regular expressions (regex)
- Can match multiple patterns using `|` (OR operator)
- Examples:
  - `^pkg/.*` - matches any file under `pkg/` directory
  - `.*\.go$` - matches any `.go` file
  - `^(pkg/build)|^(test/extended/builds)` - matches files in `pkg/build` or `test/extended/builds`

### `pipeline_skip_if_only_changed`

This annotation specifies that a test should run **unless** only files matching the pattern have changed. In other words, the test will run if any file outside the pattern changes, but will be skipped if only files matching the pattern changed.

**How it works:**
- If **all** changed files match the pattern, the test will be skipped
- If **any** changed file does not match the pattern, the test will run
- Commonly used to skip tests when only documentation or non-code files change

**How to add it:**

Add the annotation to your test configuration in the ci-operator config file:

```yaml
- always_run: false
  as: e2e-integration-test
  annotations:
    pipeline_skip_if_only_changed: ^(?:docs|\.github)/|\.md$|^(?:\.gitignore|OWNERS|OWNERS_ALIASES|PROJECT|LICENSE)$
  steps:
    workflow: openshift-e2e-test
```

**Pattern format:**
- Uses regular expressions (regex)
- Can match multiple patterns using `|` (OR operator)
- Examples:
  - `.*\.md$` - matches any `.md` file
  - `^(docs|\.github)/` - matches files in `docs/` or `.github/` directories
  - `^(?:docs|\.github)/|\.md$` - matches files in `docs/` or `.github/` directories OR any `.md` file

### Best Practices for Conditional Annotations

1. **Use `pipeline_run_if_changed` for focused tests**: Use this when a test is only relevant when specific files change (e.g., build-related tests only when build code changes).

2. **Use `pipeline_skip_if_only_changed` for broad tests**: Use this when a test should run most of the time, but can be safely skipped for documentation-only changes.

3. **Don't use both annotations**: If both `pipeline_run_if_changed` and `pipeline_skip_if_only_changed` are present, `pipeline_run_if_changed` takes precedence.

4. **Test your patterns**: Ensure your regex patterns correctly match the files you intend. You can test regex patterns using online regex testers or by examining PRs where the test should or shouldn't run.

5. **Combine with `always_run: false`**: These annotations only work with second-stage tests that have `always_run: false`.

## Manual Trigger Detection (Auto-Complement)

If you manually trigger some second-stage tests (using `/test <job-name>`) in Automatic or LGTM mode before the automatic trigger occurs, the controller **complements** your work instead of latching into manual control. When it next schedules the second stage, it:

1. Detects, from its in-cluster ProwJob cache, which second-stage jobs already have a run at the current HEAD (no extra GitHub calls are made for this check)
2. Schedules only the remaining required jobs — the "delta" — skipping the ones already triggered
3. Re-evaluates from scratch whenever the PR HEAD changes

This complements manual triggers without re-running jobs that already started. If nothing remains to schedule because every applicable job already ran for the current HEAD, the controller says so rather than claiming no tests were triggered. To re-run a specific job that already ran, use `/test <job>`; to run the delta on demand, use `/pipeline remaining`.

## Enrolling Repository

To enroll repository with the pipeline controller, you need to add it to the appropriate configuration:

### For Manual or Automatic Mode

Repository needs to be added to the main pipeline controller configuration file. Contact your platform team or CI/CD administrators to have your repository added with the desired mode (`manual` or `auto`).


### Incident-only required labels

An automatic-mode repository can temporarily require labels before the controller
schedules its existing second-stage tests:

```yaml
mode:
  trigger: auto
  required_labels:
  - acknowledge-critical-fixes-only
```

With `required_labels` absent, automatic scheduling is unchanged. With it
present, the controller schedules second-stage tests only after every listed
label is on the PR. Adding the final required label after the first stage has
passed immediately evaluates and schedules the existing second stage. This
setting does not create a ProwJob or affect direct, periodic, or postsubmit
jobs.
