package kubernetes

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/k8sbykeshed/k8s-service-validator/consts"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	scheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	poll    = 10 * time.Second
	timeout = 2 * time.Minute
)

var errPodCompleted = fmt.Errorf("pod ran to completion")

// WaitForPodRunningInNamespace waits the given timeout duration for the
// specified pod to be ready and running.
func WaitForPodRunningInNamespace(c *kubernetes.Clientset, pod *v1.Pod, pendingPodsForTaints map[string]int) error {
	if pod.Status.Phase == v1.PodRunning {
		return nil
	}
	return wait.PollImmediate(poll, timeout, podRunning(c, pod.Name, pod.Namespace, pendingPodsForTaints))
}

func podRunning(c *kubernetes.Clientset, podName, namespace string, pendingPodsForTaints map[string]int) wait.ConditionFunc {
	return func() (bool, error) {
		pod, err := c.CoreV1().Pods(namespace).Get(context.TODO(), podName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		switch pod.Status.Phase {
		case v1.PodRunning:
			return true, nil
		case v1.PodFailed, v1.PodSucceeded:
			return false, errPodCompleted
		case v1.PodPending:
			pendingPodsForTaints[podName]++
			if pendingPodsForTaints[podName] > consts.PollTimesToDeterminePendingPod {
				// determined this pod stale in pending state, may because of taints, stop waiting, delete the pod
				return true, nil
			}
			return false, nil
		}
		return false, nil
	}
}

// ExecOptions passed to ExecWithOptions
type ExecOptions struct {
	Command       []string
	Namespace     string
	PodName       string
	ContainerName string
	Stdin         io.Reader
	CaptureStdout bool
	CaptureStderr bool
	// If false, whitespace in std{err,out} will be removed.
	PreserveWhitespace bool
	Quiet              bool
}

// ExecWithOptions executes a command in the specified container,
// returning stdout, stderr and error. `options` allowed for
// additional parameters to be passed.
func ExecWithOptions(config *rest.Config, cs *kubernetes.Clientset, options *ExecOptions) (string, string, error) { // nolint
	tty := false
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(options.PodName).
		Namespace(options.Namespace).
		SubResource("exec").
		Param("container", options.ContainerName)
	req.VersionedParams(&v1.PodExecOptions{
		Container: options.ContainerName,
		Command:   options.Command,
		Stdin:     options.Stdin != nil,
		Stdout:    options.CaptureStdout,
		Stderr:    options.CaptureStderr,
		TTY:       tty,
	}, scheme.ParameterCodec)

	var stdout, stderr bytes.Buffer
	err := execute("POST", req.URL(), config, options.Stdin, &stdout, &stderr, tty)
	if options.PreserveWhitespace {
		return stdout.String(), stderr.String(), err
	}
	return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), err
}

func execute(method string, urlReq *url.URL, config *rest.Config, stdin io.Reader, stdout, stderr io.Writer, tty bool) error {
	exec, err := remotecommand.NewSPDYExecutor(config, method, urlReq)
	if err != nil {
		return err
	}
	return exec.Stream(remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
		Tty:    tty,
	})
}
