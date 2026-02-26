package apiserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	"github.com/openshift/library-go/test/library"
	exutil "github.com/openshift/origin/test/extended/util"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var _ = g.Describe("[Conformance][FeatureGate:EventTTL][Jira:\"kube-apiserver\"][Serial][Timeout:20m][Late]", func() {
	ctx := context.TODO()
	oc := exutil.NewCLIWithoutNamespace("json-patch")

	const (
		successThreshold = 1
		successInterval  = 30 * time.Second
		pollInterval     = 30 * time.Second
		timeout          = 20 * time.Minute
		targetNamespace  = "openshift-kube-apiserver"
		ttl              = int32(5)
	)

	g.BeforeEach(func() {
		isTechPreview := exutil.IsTechPreviewNoUpgrade(context.TODO(), oc.AdminConfigClient())
		if isTechPreview {
			g.Skip("skipping on tech preview")
		}

		isManagedServiceCluster, err := exutil.IsManagedServiceCluster(ctx, oc.AdminKubeClient())
		o.Expect(err).ToNot(o.HaveOccurred())
		if isManagedServiceCluster {
			g.Skip("skipping on managed service cluster")
		}
	})

	g.It("should configure eventTTLMinutes and verify", func() {
		operatorClient := oc.AdminOperatorClient()
		kubeClient := oc.AdminKubeClient()

		// Get original value for cleanup
		currentCfg, err := operatorClient.OperatorV1().KubeAPIServers().Get(ctx, "cluster", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		originalEventTTL := currentCfg.Spec.EventTTLMinutes
		g.GinkgoWriter.Printf("Original eventTTLMinutes: %d (0 means unset)\n", originalEventTTL)

		// Cleanup after test
		defer func() {
			g.By("Cleaning up eventTTLMinutes configuration")
			restore := map[string]interface{}{"spec": map[string]interface{}{}}
			if originalEventTTL == 0 {
				restore["spec"].(map[string]interface{})["eventTTLMinutes"] = nil
			} else {
				restore["spec"].(map[string]interface{})["eventTTLMinutes"] = originalEventTTL
			}
			restoreBytes, _ := json.Marshal(restore)
			_, restoreErr := operatorClient.OperatorV1().KubeAPIServers().Patch(ctx, "cluster", types.MergePatchType, restoreBytes, metav1.PatchOptions{})
			o.Expect(restoreErr).NotTo(o.HaveOccurred())
			g.GinkgoWriter.Printf("Cleanup: restored eventTTLMinutes to original value\n")
			g.By("Waiting for API server to stabilize after cleanup")
			stabilizeErr := library.WaitForPodsToStabilizeOnTheSameRevision(
				g.GinkgoT(),
				kubeClient.CoreV1().Pods(targetNamespace),
				"apiserver=true",
				successThreshold, successInterval, pollInterval, timeout,
			)
			o.Expect(stabilizeErr).NotTo(o.HaveOccurred())
			g.GinkgoWriter.Printf("Cleanup: API server stabilized after restore\n")
		}()

		g.By(fmt.Sprintf("Configuring eventTTLMinutes=%d", ttl))
		patchData := map[string]interface{}{
			"spec": map[string]interface{}{
				"eventTTLMinutes": ttl,
			},
		}
		patchBytes, err := json.Marshal(patchData)
		o.Expect(err).NotTo(o.HaveOccurred())

		_, err = operatorClient.OperatorV1().KubeAPIServers().Patch(ctx, "cluster", types.MergePatchType, patchBytes, metav1.PatchOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())

		// Verify the CR was updated
		updatedCfg, err := operatorClient.OperatorV1().KubeAPIServers().Get(ctx, "cluster", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(updatedCfg.Spec.EventTTLMinutes).To(o.Equal(ttl))
		g.GinkgoWriter.Printf("KubeAPIServer CR updated: eventTTLMinutes=%d\n", updatedCfg.Spec.EventTTLMinutes)

		g.By("Waiting for API server to stabilize with new eventTTLMinutes")
		err = library.WaitForPodsToStabilizeOnTheSameRevision(
			g.GinkgoT(),
			kubeClient.CoreV1().Pods(targetNamespace),
			"apiserver=true",
			successThreshold, successInterval, pollInterval, timeout,
		)
		o.Expect(err).NotTo(o.HaveOccurred(), "API server did not stabilize after eventTTLMinutes change")
		g.GinkgoWriter.Printf("API server stabilized with new configuration\n")

		g.By(fmt.Sprintf("Verifying event-ttl=%dm in kube-apiserver config", ttl))
		expectedTTL := fmt.Sprintf("%dm", ttl)

		var configData string
		o.Eventually(func() bool {
			configMap, err := kubeClient.CoreV1().ConfigMaps(targetNamespace).Get(ctx, "config", metav1.GetOptions{})
			if err != nil {
				g.GinkgoWriter.Printf("  Failed to get config configmap: %v\n", err)
				return false
			}
			var found bool
			configData, found = configMap.Data["config.yaml"]
			if !found {
				g.GinkgoWriter.Printf("  config.yaml not found in configmap\n")
				return false
			}
			return strings.Contains(configData, "event-ttl") && strings.Contains(configData, expectedTTL)
		}, 2*time.Minute, 5*time.Second).Should(o.BeTrue(), fmt.Sprintf("event-ttl=%s should be in config", expectedTTL))

		// Debug: print relevant part of config
		for _, line := range strings.Split(configData, "\n") {
			if strings.Contains(line, "event-ttl") || strings.Contains(line, "eventTTL") {
				g.GinkgoWriter.Printf("  Config line: %s\n", strings.TrimSpace(line))
			}
		}

		// Debug: Check kube-apiserver pod args to verify --event-ttl is set
		g.GinkgoWriter.Printf("Checking kube-apiserver pods for --event-ttl flag...\n")
		pods, err := kubeClient.CoreV1().Pods(targetNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "apiserver=true",
		})
		o.Expect(err).NotTo(o.HaveOccurred())

		for _, pod := range pods.Items {
			foundEventTTL := false
			for _, container := range pod.Spec.Containers {
				if container.Name == "kube-apiserver" {
					for _, arg := range container.Args {
						if strings.Contains(arg, fmt.Sprintf("event-ttl=%dm", ttl)) {
							g.GinkgoWriter.Printf("  Pod %s: %s\n", pod.Name, arg)
							foundEventTTL = true
						}
					}
				}
			}
			o.Expect(foundEventTTL).To(o.Equal(true), "no event-ttl in kube-apiserver pod %s", pod.Name)
		}

		g.By(fmt.Sprintf("Successfully verified eventTTLMinutes=%d configuration", ttl))
		g.By(fmt.Sprintf("Validating that events actually expire after %d minutes", ttl))

		// Create a test event
		eventName := fmt.Sprintf("ttl-test-event-%d", time.Now().Unix())
		testEvent := &corev1.Event{
			ObjectMeta: metav1.ObjectMeta{
				Name:      eventName,
				Namespace: targetNamespace,
			},
			InvolvedObject: corev1.ObjectReference{
				Kind:      "Pod",
				Namespace: targetNamespace,
				Name:      "test-pod",
				UID:       types.UID(fmt.Sprintf("uid-%d", time.Now().Unix())),
			},
			Reason:         "EventTTLTest",
			Message:        fmt.Sprintf("Test event - should expire after %dm", ttl),
			Type:           corev1.EventTypeNormal,
			Source:         corev1.EventSource{Component: "event-ttl-test"},
			FirstTimestamp: metav1.Now(),
			LastTimestamp:  metav1.Now(),
			Count:          1,
		}

		createdEvent, err := kubeClient.CoreV1().Events(targetNamespace).Create(ctx, testEvent, metav1.CreateOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		creationTime := createdEvent.CreationTimestamp.Time
		g.GinkgoWriter.Printf("Created test event: %s at %s\n", eventName, creationTime.Format(time.RFC3339))
		g.GinkgoWriter.Printf("  Event FirstTimestamp: %v\n", createdEvent.FirstTimestamp.Time.Format(time.RFC3339))
		g.GinkgoWriter.Printf("  Event LastTimestamp: %v\n", createdEvent.LastTimestamp.Time.Format(time.RFC3339))

		// Debug: Re-verify KubeAPIServer CR has eventTTLMinutes set
		currentKAS, err := operatorClient.OperatorV1().KubeAPIServers().Get(ctx, "cluster", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		g.GinkgoWriter.Printf("  KubeAPIServer.Spec.EventTTLMinutes: %d\n", currentKAS.Spec.EventTTLMinutes)

		// Poll for event deletion
		// The event GC runs periodically and may not delete events immediately after TTL expires.
		// Use TTL + 5 minutes buffer to account for GC interval variability.
		waitTimeout := time.Duration(ttl+5) * time.Minute
		expectedExpiry := creationTime.Add(time.Duration(ttl) * time.Minute)
		g.GinkgoWriter.Printf("Waiting up to %v for event to expire (expected around %s)...\n",
			waitTimeout, expectedExpiry.Format(time.RFC3339))

		pollCount := 0
		o.Eventually(func() bool {
			pollCount++
			_, err := kubeClient.CoreV1().Events(targetNamespace).Get(ctx, eventName, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) {
					g.GinkgoWriter.Printf("  Event deleted! (poll #%d)\n", pollCount)
					return true
				}
				// Print unexpected errors
				g.GinkgoWriter.Printf("  Unexpected error getting event: %v\n", err)
			}
			return false

		}, waitTimeout, 30*time.Second).Should(o.BeTrue(), fmt.Sprintf("event should be deleted after %dm TTL (waited %v)", ttl, waitTimeout))

		actualTTL := time.Since(creationTime)
		g.GinkgoWriter.Printf("Event expired after %v (expected TTL: %dm)\n", actualTTL.Round(time.Second), ttl)
		g.By(fmt.Sprintf("Successfully validated event expiration after %dm", ttl))
	})
})
