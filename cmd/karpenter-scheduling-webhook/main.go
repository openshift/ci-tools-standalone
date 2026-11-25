package main

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/bombsimon/logrusr/v3"
	"github.com/go-logr/logr"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

const (
	nvidiaGPUResource = "nvidia.com/gpu"
)

var (
	buildToleration = corev1.Toleration{
		Key:      "ci-workload",
		Operator: corev1.TolerationOpEqual,
		Value:    "builds",
		Effect:   corev1.TaintEffectNoSchedule,
	}

	longTestToleration = corev1.Toleration{
		Key:      "ci-workload",
		Operator: corev1.TolerationOpEqual,
		Value:    "longtests",
		Effect:   corev1.TaintEffectNoSchedule,
	}

	nvidiaGPUToleration = corev1.Toleration{
		Key:      nvidiaGPUResource,
		Operator: corev1.TolerationOpEqual,
		Value:    "true",
		Effect:   corev1.TaintEffectNoSchedule,
	}

	KVMVirtToleration = corev1.Toleration{
		Key:      "ci-workload",
		Operator: corev1.TolerationOpEqual,
		Value:    "virt-launcher",
		Effect:   corev1.TaintEffectNoSchedule,
	}

	opts = options{}

	rootCmd = &cobra.Command{
		Use:   "karpenter-scheduling-webhook",
		Short: "Controls where pods will be scheduled by Karpenter",
		Long: `Controls where pods will be scheduled by Karpenter.

Example:
$ karpenter-scheduling-webhook --cert-dir=<cert-dir> --port=443`,
		RunE: RunE,
	}
)

func init() {
	rootCmd.Flags().StringVar(&opts.certDir, "cert-dir", "", "A folder holding the server private key and and certificate for TLS")
	rootCmd.Flags().StringVar(&opts.healthProbeAddr, "health-probe-addr", ":8081", "Health probe binding address <addr>:<port>. Default to :8081")
	rootCmd.Flags().IntVar(&opts.port, "port", 0, "Port the server will listen on")
}

func setupLogger() logr.Logger {
	innerLogger := logrus.New()
	innerLogger.Formatter = &logrus.JSONFormatter{}
	log.SetLogger(logrusr.New(innerLogger))
	return log.Log
}

type options struct {
	certDir         string
	port            int
	healthProbeAddr string
}

type podDefaults struct{}

func (*podDefaults) Default(ctx context.Context, pod *corev1.Pod) error {

	// Ignore pods that are not tests
	if !strings.HasPrefix(pod.Namespace, "ci-op-") && !strings.HasPrefix(pod.Namespace, "ci-ln-") {
		return nil
	}

	logger := log.FromContext(ctx)
	logger = logger.WithValues("pod", fmt.Sprintf("%s/%s", pod.Namespace, pod.Name))

	nodeSelector := pod.Spec.NodeSelector
	if nodeSelector == nil {
		nodeSelector = make(map[string]string)
	}

	if needBuildToleration(pod.Name) {
		nodeSelector["ci-workload"] = string("builds")
		pod.Spec.NodeSelector = nodeSelector
		addToleration(logger, pod, buildToleration)
		return nil
	}

	if needLongTestToleration(pod.Name) {
		nodeSelector["ci-workload"] = string("longtests")
		pod.Spec.NodeSelector = nodeSelector
		addToleration(logger, pod, longTestToleration)
	}

	// Check if any custom selector is needed
	gpuNeeded := hasNvidaGPURequest(logger, pod.Spec.InitContainers)
	KVMVirtNeeded := hasKVMVirtRequest(logger, pod.Spec.InitContainers)

	if !gpuNeeded {
		gpuNeeded = hasNvidaGPURequest(logger, pod.Spec.Containers)
	}

	if gpuNeeded {
		addToleration(logger, pod, nvidiaGPUToleration)
	}

	if !KVMVirtNeeded {
		KVMVirtNeeded = hasKVMVirtRequest(logger, pod.Spec.Containers)
	}

	if KVMVirtNeeded {
		addToleration(logger, pod, KVMVirtToleration)
	}

	return nil
}

func hasNvidaGPURequest(logger logr.Logger, containers []corev1.Container) bool {
	for i := range containers {
		c := &containers[i]
		if needNvidiaGPU(c.Resources) {
			logger.Info("Request Nvidia GPU", "container", c.Name)
			return true
		}
	}
	return false
}

func hasKVMVirtRequest(logger logr.Logger, containers []corev1.Container) bool {
	for i := range containers {
		c := &containers[i]
		if needKVMVirt(c.Resources) {
			logger.Info("Request KVM Virt", "container", c.Name)
			return true
		}
	}
	return false
}

func needKVMVirt(requirement corev1.ResourceRequirements) bool {
	_, requestExists := requirement.Requests["devices.kubevirt.io/kvm"]
	_, limitExists := requirement.Limits["devices.kubevirt.io/kvm"]
	return requestExists || limitExists
}

func addToleration(logger logr.Logger, pod *corev1.Pod, toleration corev1.Toleration) {
	if !slices.Contains(pod.Spec.Tolerations, toleration) {
		pod.Spec.Tolerations = append(pod.Spec.Tolerations, toleration)
		logger.Info(fmt.Sprintf("Added %s toleration to pod %s at %s namespace", toleration.Key, pod.Name, pod.Namespace))
	} else {
		logger.Info(fmt.Sprintf("%s toleration already exists on pod %s at %s namespace", toleration.Key, pod.Name, pod.Namespace))
	}
}

func needNvidiaGPU(requirement corev1.ResourceRequirements) bool {
	_, requestExists := requirement.Requests[nvidiaGPUResource]
	_, limitExists := requirement.Limits[nvidiaGPUResource]
	return requestExists || limitExists
}

func needBuildToleration(podName string) bool {
	return strings.HasSuffix(podName, "-build")
}

func needLongTestToleration(podName string) bool {
	return strings.HasPrefix(podName, "release-analysis-aggregator-") ||
		strings.HasPrefix(podName, "e2e-aws-upgrade") ||
		strings.HasPrefix(podName, "rpm-repo") ||
		strings.HasPrefix(podName, "osde2e-stage") ||
		strings.HasPrefix(podName, "e2e-aws-cnv") ||
		strings.Contains(podName, "ovn-upgrade-ipi") ||
		strings.Contains(podName, "ovn-upgrade-ovn") ||
		strings.Contains(podName, "ovn-upgrade-openshift-e2e-test")
}

func startWebhookServer(ctx context.Context, logger *logr.Logger, o *options, cfg *rest.Config) error {
	logger.Info("Setting up manager")
	mgr, err := manager.New(cfg, manager.Options{
		HealthProbeBindAddress: o.healthProbeAddr,
		WebhookServer: webhook.NewServer(webhook.Options{
			CertDir: o.certDir,
			Port:    o.port,
		}),
	})
	if err != nil {
		logger.Error(err, "Unable to set up manager")
		return err
	}

	if err := mgr.AddHealthzCheck("healthz", func(req *http.Request) error { return nil }); err != nil {
		logger.Error(err, "Unable to set up healthz endpoint")
		return err
	}

	if err := mgr.AddReadyzCheck("readyz", func(req *http.Request) error { return nil }); err != nil {
		logger.Error(err, "Unable to set up readyz endpoint")
		return err
	}

	logger.WithValues("Addr", o.healthProbeAddr).Info("Serving healthiness probes")

	if err := builder.WebhookManagedBy(mgr, &corev1.Pod{}).
		WithDefaulter(&podDefaults{}).
		Complete(); err != nil {
		logger.Error(err, "Unable to build pod webhook")
		return err
	}

	logger.Info("Starting manager")
	if err := mgr.Start(ctx); err != nil {
		logger.Error(err, "Unable to start manager")
		return err
	}

	return nil
}

func RunE(cmd *cobra.Command, args []string) error {
	logger := setupLogger().WithName("karpenter-scheduling")
	logger.Info("Starting the webhook")

	cfg, err := config.GetConfig()
	if err != nil {
		return fmt.Errorf("get cluster config: %w", err)
	}

	return startWebhookServer(cmd.Context(), &logger, &opts, cfg)
}

func main() {
	cobra.CheckErr(rootCmd.ExecuteContext(signals.SetupSignalHandler()))
}
