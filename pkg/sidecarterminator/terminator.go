package sidecarterminator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"syscall"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog"
)

const (
	SidecarTerminatorContainerNamePrefix = "sidecar-terminator"
	SidecarTerminatorContainerNameRegex  = "sidecar-terminator-([a-zA-Z0-9-]+)-([0-9]+)"
)

// SidecarTerminator defines an instance of the sidecar terminator.
type SidecarTerminator struct {
	config    *rest.Config
	clientset *kubernetes.Clientset

	eventHandler *sidecarTerminatorEventHandler

	terminatorImage string
	sidecars        map[string]int
	namespaces      []string
}

// NewSidecarTerminator returns a new SidecarTerminator instance.
func NewSidecarTerminator(config *rest.Config, clientset *kubernetes.Clientset, terminatorImage string, sidecarsstr, namespaces []string) (*SidecarTerminator, error) {
	if config == nil {
		return nil, errors.New("config cannot be nil")
	}

	if clientset == nil {
		return nil, errors.New("clientset cannot be nil")
	}

	if terminatorImage == "" {
		return nil, errors.New("sidecarTerminatorImage cannot be empty")
	}

	sidecars := map[string]int{}
	for _, sidecar := range sidecarsstr {
		comp := strings.Split(sidecar, "=")

		// If the name of the sidecar is too long, we won't be able to generate a
		// a sidecar container with a name that is a DNS_LABEL of 63 characters
		// This is optimistic in hoping that 1 digit (0-9, or 10 containers) is enough
		// to terminate the sidecar.
		// (length of sidecar name) + (length of prefix) + (2 dashes) + (1 digit)
		if len([]rune(comp[0]))+len([]rune(SidecarTerminatorContainerNamePrefix))+3 > 63 {
			return nil, fmt.Errorf("%q sidecar name is too long to be able to create a %s", comp[0], SidecarTerminatorContainerNamePrefix)
		}

		if len(comp) == 1 {
			sidecars[comp[0]] = int(syscall.SIGTERM)
		} else if len(comp) == 2 {
			num, err := strconv.Atoi(comp[1])
			if err != nil {
				return nil, err
			}

			sidecars[comp[0]] = num
		} else {
			return nil, fmt.Errorf("incorrect sidecar container name format: %s", sidecar)
		}
	}

	return &SidecarTerminator{
		config:          config,
		clientset:       clientset,
		terminatorImage: terminatorImage,
		sidecars:        sidecars,
		namespaces:      namespaces,
	}, nil
}

func (st *SidecarTerminator) setupInformerForNamespace(ctx context.Context, namespace string) error {
	if namespace == v1.NamespaceAll {
		klog.Info("starting shared informer")
	} else {
		klog.Infof("starting shared informer for namespace %q", namespace)
	}

	factory := informers.NewFilteredSharedInformerFactory(
		st.clientset,
		time.Minute*10,
		namespace,
		nil,
	)

	factory.Core().V1().Pods().Informer().AddEventHandler(st.eventHandler)
	factory.Start(ctx.Done())
	for _, ok := range factory.WaitForCacheSync(nil) {
		if !ok {
			return errors.New("timed out waiting for controller caches to sync")
		}
	}

	return nil
}

// Run runs the sidecar terminator.
// TODO: Ensure this is only called once..
func (st *SidecarTerminator) Run(ctx context.Context) error {
	klog.Info("starting sidecar terminator")

	// Setup event handler
	st.eventHandler = &sidecarTerminatorEventHandler{
		st: st,
	}

	// Setup shared informer factory
	if len(st.namespaces) == 0 {
		if err := st.setupInformerForNamespace(ctx, metav1.NamespaceAll); err != nil {
			return err
		}
	} else {
		for _, namespace := range st.namespaces {
			if err := st.setupInformerForNamespace(ctx, namespace); err != nil {
				return err
			}
		}
	}

	<-ctx.Done()
	klog.Info("terminating sidecar terminator")
	return nil
}

func (st *SidecarTerminator) terminate(pod *v1.Pod) error {
	klog.Infof("Found running sidecar containers in %s", podName(pod))

	// Terminate the sidecar
	for _, sidecar := range pod.Status.ContainerStatuses {
		if isSidecarContainer(sidecar.Name, st.sidecars) && sidecar.State.Running != nil {

			// TODO: Add ability to kill the proper process
			// May require looking into the OCI image to extract the entrypoint if not
			// available via the containers' command.
			if pod.Spec.ShareProcessNamespace != nil && *pod.Spec.ShareProcessNamespace {
				klog.Error("Containers are sharing process namespace: ending process 1 will not end sidecars.")
				return fmt.Errorf("unable to end sidecar %s in pod %s using shareProcessNamespace", sidecar.Name, podName(pod))
			}

			if !sidecar.Ready {
				klog.Infof("%s sidecar is not ready, termination will wait", sidecar.Name)
				return nil
			}

			// No sidecar-terminator deployed yet
			if !hasSidecarTerminatorContainer(pod, sidecar) {
				klog.Infof("Terminating sidecar %s from %s with signal %d", sidecar.Name, podName(pod), st.sidecars[sidecar.Name])

				securityContext, err := getSidecarSecurityContext(pod, sidecar.Name)
				if err != nil {
					return err
				}

				pod.Spec.EphemeralContainers = append(pod.Spec.EphemeralContainers, st.generateSidecarTerminatorEphemeralContainer(sidecar, securityContext, 0))

				_, err = st.clientset.CoreV1().Pods(pod.Namespace).UpdateEphemeralContainers(context.TODO(), pod.Name, pod, metav1.UpdateOptions{})
				if err != nil {
					klog.Error(err)
					return err
				}
			}

			sidecarTerminatorStatuses := getSidecarTerminatorStatuses(pod.Status.EphemeralContainerStatuses)

			if len(sidecarTerminatorStatuses) > 0 && hasContainersTerminated(sidecarTerminatorStatuses) {
				mostRecentTerminatorStatus := getMostRecentSidecarTerminatorStatus(sidecarTerminatorStatuses)

				// Has errored out. Print out logs for more context.
				if mostRecentTerminatorStatus.State.Terminated.ExitCode != 0 {
					klog.Infof("%s has ended with return code of %d", mostRecentTerminatorStatus.Name, mostRecentTerminatorStatus.State.Terminated.ExitCode)
					logs, err := st.getContainerLogs(pod, mostRecentTerminatorStatus.Name)
					if err != nil {
						klog.Errorf("unable to get logs: %q", err)
					}
					// klog.Infof(logs)
					klog.Infof("outputting logs for %s\n\n%s\n", mostRecentTerminatorStatus.Name, logs)
				}

				//TODO: Add retry mechanism for failures or if pod is still running but command was run successfully.
				//TODO: If configured signal isn't working, final attempt should use a sigkill.
			}
		}
	}

	return nil
}

func (st *SidecarTerminator) generateSidecarTerminatorEphemeralContainer(sidecar v1.ContainerStatus, securityContext *v1.SecurityContext, num int) v1.EphemeralContainer {
	return v1.EphemeralContainer{
		TargetContainerName: sidecar.Name,
		EphemeralContainerCommon: v1.EphemeralContainerCommon{
			Name:  fmt.Sprintf("%s-%s-%d", SidecarTerminatorContainerNamePrefix, sidecar.Name, num),
			Image: st.terminatorImage,
			Command: []string{
				"kill",
			},
			Args: []string{
				fmt.Sprintf("-%d", st.sidecars[sidecar.Name]),
				"1",
			},
			ImagePullPolicy: v1.PullAlways,
			// SecurityContext: securityContext,
		},
	}
}

func (st *SidecarTerminator) getContainerLogs(pod *v1.Pod, containerName string) (string, error) {
	logsRequest := st.clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &v1.PodLogOptions{
		Container: containerName,
	})

	podLogs, err := logsRequest.Stream(context.TODO())
	if err != nil {
		klog.Fatal("error in opening stream")
	}
	defer podLogs.Close()

	buf := new(bytes.Buffer)
	_, err = io.Copy(buf, podLogs)
	if err != nil {
		klog.Errorf("error retrieving logs from container %s in %s/%s", pod.Namespace, pod.Name, containerName)
		return "", nil
	}

	logs := buf.String()

	return logs, nil
}
