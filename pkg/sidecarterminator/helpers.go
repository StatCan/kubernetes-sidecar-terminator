package sidecarterminator

import (
	"fmt"
	"regexp"
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TODO: make this read-only
// Put into sidecarTerminator struct?
var sidecarTerminatorNameRegex = regexp.MustCompile(fmt.Sprintf("%s-([a-zA-Z0-9-]+)-([0-9]+)", SidecarTerminatorContainerNamePrefix))

func podName(pod *v1.Pod) string {
	return fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
}

func isOwnedByJob(references []metav1.OwnerReference) bool {
	for _, ref := range references {
		// Jobs
		if ref.APIVersion == "batch/v1" && ref.Kind == "Job" {
			return true
		}

		// Argo Workflows
		if strings.HasPrefix(ref.APIVersion, "argoproj.io/") && ref.Kind == "Workflow" {
			return true
		}
	}

	return false
}

func isSidecarContainer(name string, sidecars map[string]int) bool {
	for containerName := range sidecars {
		if name == containerName {
			return true
		}
	}

	return false
}

func isCompleted(pod *v1.Pod, sidecars map[string]int) bool {
	if pod.Status.Phase == v1.PodRunning {
		complete := true

		for _, containerStatus := range pod.Status.ContainerStatuses {
			// Ignore the status of sidecar containers
			if isSidecarContainer(containerStatus.Name, sidecars) {
				continue
			}

			// Check that the container is terminated
			containerComplete := containerStatus.State.Terminated != nil

			// If the restart policy is not Never, then let's ensure the container has exited with a successful error code (exit code 0)
			if pod.Spec.RestartPolicy != v1.RestartPolicyNever {
				containerComplete = containerComplete && containerStatus.State.Terminated.ExitCode == 0
			}

			complete = complete && containerComplete
		}

		return complete
	}

	return false
}

func hasSidecarTerminatorContainer(pod *v1.Pod, sidecar v1.ContainerStatus) bool {
	for _, ephCont := range pod.Spec.EphemeralContainers {
		if sidecarTerminatorNameRegex.MatchString(ephCont.Name) {
			return true
		}
	}

	return false
}

func hasContainersTerminated(containerStatuses []v1.ContainerStatus) bool {
	var terminated = true
	for _, ephCont := range containerStatuses {
		terminated = terminated && ephCont.State.Terminated != nil
	}

	return terminated
}

func getSidecarTerminatorStatuses(containerStatuses []v1.ContainerStatus) []v1.ContainerStatus {
	statuses := make([]v1.ContainerStatus, 0, len(containerStatuses))
	for _, ephemeralContainerStatus := range containerStatuses {
		if sidecarTerminatorNameRegex.MatchString(ephemeralContainerStatus.Name) {
			statuses = append(statuses, ephemeralContainerStatus)
		}
	}
	return statuses
}

func getMostRecentSidecarTerminatorStatus(containerStatuses []v1.ContainerStatus) v1.ContainerStatus {
	// This is a lazy implementation that assumes the server
	// returns statuses of containers in order of creation.
	// May want to use the suffix number of the name to
	// determine the most recent container.
	return containerStatuses[len(containerStatuses)-1]
}

func getSidecarSecurityContext(pod *v1.Pod, sidecar string) (*v1.SecurityContext, error) {
	for _, container := range pod.Spec.Containers {
		if container.Name == sidecar {
			return container.SecurityContext, nil
		}
	}

	return nil, fmt.Errorf("unable to get security context of %s sidecar in %s/%s", sidecar, pod.Namespace, pod.Name)
}
