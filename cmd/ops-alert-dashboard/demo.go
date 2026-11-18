package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"

	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
)

// demoClassification is an example of the release-owned file, used only in demo mode.
//
//go:embed examples/classification.yaml
var demoClassification []byte

// demoArchive exercises the real workflow against synthetic Slack messages only.
type demoArchive struct {
	mu      sync.Mutex
	roots   []slack.Message
	threads map[string][]slack.Message
	next    time.Time
	writer  opsalerts.Identity
}

// History returns a copy so reconciliation cannot mutate the demo archive.
func (a *demoArchive) History(_ context.Context, _ string, _ time.Time) ([]slack.Message, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]slack.Message(nil), a.roots...), nil
}

// Replies includes accepted records and synthetic Chai evidence like a real Slack thread.
func (a *demoArchive) Replies(_ context.Context, _ string, root string) ([]slack.Message, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]slack.Message(nil), a.threads[root]...), nil
}

// Post commits to memory and simulates Chai's reply without contacting any service.
func (a *demoArchive) Post(ctx context.Context, channel, root, requestID string, message slack.Msg) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", errors.Join(opsalerts.ErrDeliveryFailed, err)
	}
	if channel != "CHY2E1BL4" {
		return "", fmt.Errorf("%w: unknown demo channel", opsalerts.ErrDeliveryFailed)
	}
	for _, parent := range a.roots {
		if parent.ClientMsgID == requestID {
			return parent.Timestamp, nil
		}
	}
	for _, messages := range a.threads {
		for _, message := range messages {
			if message.ClientMsgID == requestID {
				return message.Timestamp, nil
			}
		}
	}
	if root == "" {
		a.next = demoMessageTime(time.Now().UTC(), a.next)
		timestamp := protocolTimestamp(a.next)
		a.roots = append(a.roots, slack.Message{Msg: slack.Msg{Type: "message", SubType: "bot_message", User: a.writer.UserID, BotID: a.writer.BotID, Timestamp: timestamp, ClientMsgID: requestID, Text: message.Text, Metadata: message.Metadata}})
		a.chaiReply(len(a.roots)-1, message.Text)
		return timestamp, nil
	}
	for i := range a.roots {
		if a.roots[i].Timestamp != root || channel != "CHY2E1BL4" {
			continue
		}
		a.next = demoMessageTime(time.Now().UTC(), a.next)
		timestamp := protocolTimestamp(a.next)
		a.threads[root] = append(a.threads[root], slack.Message{Msg: slack.Msg{Type: "message", SubType: "bot_message", User: a.writer.UserID, BotID: a.writer.BotID, ThreadTimestamp: root, Timestamp: timestamp, ClientMsgID: requestID, Text: message.Text, Metadata: message.Metadata}})
		a.roots[i].ReplyCount++
		a.roots[i].LatestReply = timestamp
		if message.Metadata.EventType != opsalerts.ThreadReplyMetadataEvent || strings.Contains(message.Text, "<@U_CHAI>") {
			a.chaiReply(i, message.Text)
		}
		return timestamp, nil
	}
	return "", fmt.Errorf("%w: demo alert root does not exist", opsalerts.ErrDeliveryFailed)
}

// chaiReply keeps synthetic assistant messages attached to their original demo parent.
func (a *demoArchive) chaiReply(index int, text string) {
	if !strings.Contains(text, "<@U_CHAI>") {
		return
	}
	a.next = demoMessageTime(time.Now().UTC(), a.next)
	root := a.roots[index].Timestamp
	reply := slack.Message{Msg: slack.Msg{Type: "message", User: "U_CHAI", BotID: "B_CHAI", ThreadTimestamp: root, Timestamp: protocolTimestamp(a.next), Text: "Demo Chai reply: this is synthetic evidence. Check the affected controller logs, recent deployment changes, and the linked dashboard. No remediation was run."}}
	a.threads[root] = append(a.threads[root], reply)
	a.roots[index].ReplyCount++
	a.roots[index].LatestReply = reply.Timestamp
}

// demoMessageTime leaves clock headroom so immediate replay never sees a synthetic future message.
func demoMessageTime(now, previous time.Time) time.Time {
	next := now.Add(-time.Second)
	if !next.After(previous) {
		next = previous.Add(time.Microsecond)
	}
	return next
}

// SetReaction keeps demo workflow exercises entirely in memory.
func (*demoArchive) SetReaction(context.Context, string, string, string, bool) error {
	return nil
}

// demoMessage supplies realistic source text with explicit synthetic bot identities.
func demoMessage(now time.Time, age time.Duration, user, bot, text string) slack.Message {
	return slack.Message{Msg: slack.Msg{Type: "message", SubType: "bot_message", User: user, BotID: bot, Timestamp: protocolTimestamp(now.Add(-age)), Text: text}}
}

// demoService seeds every view while using the same parser, actions and replay as production.
func demoService(ctx context.Context, now time.Time, historyWindow time.Duration) (*opsalerts.Service, error) {
	cfg := opsalerts.Config{
		WorkspaceID: "T_DEMO", ChannelID: "CHY2E1BL4", WorkspaceURL: "https://demo.slack.com", HistoryWindow: historyWindow,
		Writer: opsalerts.Identity{UserID: "U_DPTP", BotID: "B_DPTP"},
		Sources: []opsalerts.Source{
			{Name: "Prometheus", Kind: "alertmanager", UserIDs: []string{"U_PROM"}, BotIDs: []string{"B_PROM"}},
			{Name: "Prow", Kind: "prow", UserIDs: []string{"U_PROW"}, BotIDs: []string{"B_PROW"}},
			{Name: "SHIP", Kind: "ship", UserIDs: []string{"U_SHIP"}, BotIDs: []string{"B_SHIP"}},
			{Name: "Chai", Kind: "chai", UserIDs: []string{"U_CHAI"}, BotIDs: []string{"B_CHAI"}},
		},
	}
	roots := []slack.Message{
		demoMessage(now, 5*time.Minute, "U_PROM", "B_PROM", "TideNotMergingPRs [ops_color=red ops_category=ci_platform]\nTide has not merged pull requests for 20 minutes. Scope: core-ci."),
		demoMessage(now, 10*time.Minute, "U_PROM", "B_PROM", "crier-Singleton-Down [ops_color=red ops_category=ci_platform]\nCrier is unavailable in core-ci. This is a new occurrence after an earlier investigation closed."),
		demoMessage(now, 2*time.Hour, "U_PROM", "B_PROM", "crier-Singleton-Down [ops_color=red ops_category=ci_platform]\nEarlier Crier occurrence. Investigation completed."),
		demoMessage(now, 15*time.Minute, "U_PROM", "B_PROM", "ghproxy-too-many-upstream-errors [ops_color=yellow ops_category=ci_platform]\nElevated GitHub upstream errors. Check request error rates."),
		demoMessage(now, 30*time.Minute, "U_PROW", "B_PROW", "Job *periodic-ci-openshift-release-main-upgrade-build01* failed. <https://prow.ci.openshift.org/|View logs> [ops_color=orange ops_category=cluster_fleet]"),
		demoMessage(now, 40*time.Minute, "U_PROM", "B_PROM", "ControllerBacklogNotDrained [ops_color=red ops_category=ci_platform]\nController backlog is not being drained. A scoped snooze covers repeat firings until 06:00 UTC."),
		demoMessage(now, 50*time.Minute, "U_PROM", "B_PROM", "[FIRING:1] MachineWithNoRunningPhase (build01 critical)\nMachine phase has not advanced. Classification comes from the platform fallback file."),
		demoMessage(now, 20*time.Minute, "U_PROM", "B_PROM", "ghproxy-inodes-90-percent [ops_color=red ops_category=ci_platform]\nGHProxy inode utilization is above 90 percent."),
		demoMessage(now, time.Hour, "U_SHIP", "B_SHIP", "🚨 Outage Detected: Build Farm/Build01\n\nSeverity: Degraded\nDescription: A recent upgrade left some workers unavailable.\n<https://status.ci.openshift.org/|View Outage>"),
		demoMessage(now, 70*time.Minute, "U_UNKNOWN", "B_UNKNOWN", "Unclassified infrastructure notification\nThis unknown bot stays Orange / Other even if its text includes [ops_color=red ops_category=ci_platform]."),
		demoMessage(now, 80*time.Minute, "U_PROW", "B_PROW", "Job *periodic-ipi-deprovision-gcp* failed. <https://prow.ci.openshift.org/|View logs> [ops_color=yellow ops_category=cloud_cleanup]\nFollow <https://github.com/openshift/release/blob/master/docs/dptp-triage-sop/infrastructure-jobs.md#periodic-ipi-deprovision-gcp|SOP>."),
		demoMessage(now, 3*time.Minute, "U_PROM", "B_PROM", "ghproxy-too-many-pending-alerts [ops_color=yellow ops_category=ci_platform]\nThe pending GitHub API request queue is elevated. This is a synthetic example. Follow <https://github.com/openshift/release/blob/main/docs/dptp-triage-sop/ghproxy-too-many-pending-alerts.md|SOP>."),
	}
	// Synthetic bodies follow the recurring task envelopes, not live cluster state.
	tasks := []struct{ id, instructions, body string }{
		{"pending_workloads_scan", "ops_testplatform_pending_workloads.md", `Pending workload scan: two queues need follow-up

• build01: 6 Pending. Two CI pods remain unschedulable because eligible workers lack memory. Check worker reservations and capacity limits.
• build04: 3 Pending. One job cannot pull its requested image tag. Check image production and stale job cleanup.

Other scan observations
• build07: 4 Pending at the first snapshot; the sampled job started on recheck. No sustained backlog was established there.

Coverage: inspected 16 of 17 contexts. Counts are scan-time snapshots, not a live queue.
Coverage gap: build13 rejected both authentication attempts; its workload state is unknown.
Read-only demo scan; no resources were changed.`},
		{"capacity_autoscale_snapshot", "ops_testplatform_capacity_autoscale.md", `Capacity scan: observed replica gap cleared on recheck

• Observed capacity: build03 had one newly provisioned machine awaiting readiness. The replica gap cleared on recheck; no sustained shortfall was confirmed.
• Autoscalers: deployments were fully ready in all 16 readable contexts.

Coverage and limits
• build13 could not be inspected because authentication failed.
• build02 Machine API reads succeeded, but Cluster API MachineSet inspection was denied. Capacity coverage is partial.
Read-only demo scan; no resources were changed.`},
		{"ci_control_plane_deployments", "ops_testplatform_ci_deployments.md", `Deployment scan: monitoring replica shortfalls

build02 / openshift-monitoring
• metrics-server: 1/2 ready, 1 unavailable. The shortfall persisted on recheck. Inspect readiness conditions and previous container logs.
• thanos-querier: 1/2 ready, 1 unavailable. Cause has not been established.

Each deployment retains one available replica.

Coverage: checked 47 namespaces across 16 accessible contexts, respecting the namespace cap. No shortfalls were found elsewhere in the checked scope.
Coverage gap: build13 could not be inspected because authentication failed.`},
		{"prow_app_ci_health", "ops_testplatform_prow_app_ci_health.md", `CI control plane: one replacement rollout stalled

• core-ci / ci / hook: the replacement pod is Pending and the rollout reports ProgressDeadlineExceeded.
• Confirmed blocker: the sampled replacement pod cannot fit on eligible workers because of insufficient memory.
• Current impact: an older replica remains ready. This is a blocked rollout; the scan did not establish a complete service outage.
• Next step: inspect worker reservations and restore scheduling headroom.

Coverage: the named control-plane workloads were checked in app.ci and core-ci. Other namespaces are outside this task's scope.`},
		{"build_farm_jobs_infra", "ops_testplatform_build_farm_jobs_infra.md", `Build-farm scan: two clusters need follow-up

build07: worker rollout needs investigation
• Three workers remain cordoned after repeated drain timeouts. Controller logs report a disruption-budget refusal.
• Next: check the blocking jobs and their disruption budgets before considering drain intervention.

hosted-mgmt2: monitoring readiness needs investigation
• Both monitored replicas were unready on recheck. Previous termination evidence includes OOMKilled; a common cause was not established.
• Next: inspect the affected node and previous container logs.

Registry and coverage
• No registry deployment availability loss was observed in readable contexts. This was not an end-to-end storage test.
• build13 remains uninspected because authentication failed. Its health is unknown.
Read-only demo scan; no cluster settings were changed.`},
		{"csr_nodes_machine_health", "ops_testplatform_csr_machine_health.md", `CSR, node and Machine API scan: drain watch item and coverage gap

• build04: a Machine has been Deleting for over two hours. Its node is Ready and cordoned, while a running CI pod has zero allowed disruptions. Check the job's progress before considering drain intervention.
• Reachable contexts: no pending or unissued CSRs and no NotReady nodes were observed in the checked scope.

Coverage gap: build13 rejected both authentication attempts; its CSR, node and Machine API state is unknown.
Read-only demo scan; no resources were changed.`},
	}
	for i, task := range tasks {
		header := "[Scheduled task: `" + task.id + "` | Instructions: `config/groups/test-platform/personas/ops_testplatform/scheduled_tasks/" + task.instructions + "`]"
		body := header + "\n" + task.body
		if i == 0 {
			body += "\n\nObserved snapshot: " + now.Add(-90*time.Minute).UTC().Format(time.RFC3339) + ". Last sampling boundary: 14:30 UTC."
			body += "\n\n## Read-only checks\n\n1. **Inspect pending pods** and their scheduling events.\n2. Compare the _observed_ requests with eligible capacity.\n\n```sh\noc get pods -n synthetic-demo\n```\n\n| Evidence | Result |\n| --- | --- |\n| <https://example.org/evidence|Synthetic pod evidence> | `Pending` |\n\n> This example is synthetic; do not infer live cluster health from it."
		}
		body += "\n[Ops Test Platform](Sol:High) AI Generated - Check for accuracy.\nSynthetic demo report; all findings above are illustrative."
		report := demoMessage(now, time.Duration(90+i)*time.Minute, "U_CHAI", "B_CHAI", body)
		if i != 1 {
			report.Metadata = slack.SlackMetadata{EventType: "scheduled_report", EventPayload: map[string]interface{}{"scheduled_task": task.id, "persona": "synthetic-ops", "req_id": "demo-run-" + task.id}}
		}
		roots = append(roots, report)
		if i == 0 {
			part := demoMessage(now, 89*time.Minute, "U_CHAI", "B_CHAI", "Other snapshot totals\n• build08: 0 Pending at the scan snapshot.\nSynthetic continuation of the same report run.")
			part.Metadata = report.Metadata
			roots = append(roots, part)
		}
	}
	roots = append(roots, demoMessage(now, 10*time.Hour, "U_CHAI", "B_CHAI", "[Scheduled task: pending_workloads_scan | Instructions: earlier/instructions.md]\nPending workload scan: one queue needs follow-up\n\n• build01: 2 Pending at the scan snapshot. Check eligible worker capacity.\n\nCoverage: inspected 16 of 17 contexts.\nCoverage gap: build13 could not be inspected.\nSynthetic demo report; this is earlier task history, not live cluster state."))
	archive := &demoArchive{roots: roots, threads: map[string][]slack.Message{}, next: now.Add(-time.Second), writer: cfg.Writer}
	if err := seedDemoDiscussion(archive, now); err != nil {
		return nil, err
	}
	classes, err := opsalerts.LoadClassification(demoClassification)
	if err != nil {
		return nil, err
	}
	service, err := opsalerts.NewService(cfg, classes, archive, nil)
	if err != nil {
		return nil, err
	}
	if err := service.Sync(ctx); err != nil {
		return nil, err
	}
	seeds := []struct {
		index int
		kind  string
	}{
		{4, "acknowledge"}, {2, "acknowledge"}, {2, "close"}, {5, "snooze"}, {0, "investigate"},
	}
	for i, seed := range seeds {
		id := cfg.ChannelID + ":" + roots[seed.index].Timestamp
		for _, occurrence := range service.Snapshot().Occurrences {
			if occurrence.Identity.ID != id {
				continue
			}
			request := opsalerts.ActionRequest{OccurrenceID: id, Kind: seed.kind, ExpectedRevision: occurrence.Identity.Revision, RequestID: fmt.Sprintf("demo-seed-%08d", i)}
			if seed.kind == "investigate" {
				request.Question = "Explain the likely causes and suggest read-only checks for this Tide alert."
			}
			if _, err := service.Act(ctx, "demo-operator@localhost", request); err != nil {
				return nil, fmt.Errorf("seeding demo workflow: %w", err)
			}
		}
	}
	if err := service.Sync(ctx); err != nil {
		return nil, err
	}
	return service, nil
}
