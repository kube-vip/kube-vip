package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func GetLogs(ctx context.Context, client kubernetes.Interface, tempDirPath string) error {
	if os.Getenv("E2E_KEEP_LOGS") != "true" {
		if err := os.RemoveAll(tempDirPath); err != nil {
			return fmt.Errorf("failed to remove temporary directory %q: %w", tempDirPath, err)
		}
		return nil
	}

	intCtx, cancel := context.WithTimeout(ctx, time.Second*20)
	defer cancel()

	path := filepath.Join(tempDirPath, "pods.json")
	fpods, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create file %q: %w", path, err)
	}
	defer fpods.Close()

	// list pods
	pods, err := client.CoreV1().Pods("").List(intCtx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to get pods: %w", err)
	}
	data, err := json.Marshal(pods)
	if err != nil {
		return fmt.Errorf("failed to marshal pods data: %w", err)
	}
	if _, err := fpods.Write(data); err != nil {
		return fmt.Errorf("failed to save pods: %w", err)
	}

	// list services
	path = filepath.Join(tempDirPath, "services.json")
	fsvcs, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create file %q: %w", path, err)
	}
	defer fpods.Close()

	svcs, err := client.CoreV1().Services("").List(intCtx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to get pods: %w", err)
	}
	data, err = json.Marshal(svcs)
	if err != nil {
		return fmt.Errorf("failed to marshal pods data: %w", err)
	}
	if _, err := fsvcs.Write(data); err != nil {
		return fmt.Errorf("failed to save pods: %w", err)
	}

	// list leases
	path = filepath.Join(tempDirPath, "leases.json")
	fleases, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create file %q: %w", path, err)
	}
	defer fpods.Close()

	leases, err := client.CoordinationV1().Leases("").List(intCtx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to get leases: %w", err)
	}
	data, err = json.Marshal(leases)
	if err != nil {
		return fmt.Errorf("failed to marshal leases data: %w", err)
	}
	if _, err := fleases.Write(data); err != nil {
		return fmt.Errorf("failed to save leases: %w", err)
	}

	// get kube-vip pods
	listOptions := metav1.ListOptions{
		LabelSelector: "app=kube-vip",
	}

	var kvpods *corev1.PodList
	kvpods, err = client.CoreV1().Pods("").List(intCtx, listOptions)
	if err != nil {
		return fmt.Errorf("failed to list pods: %w", err)
	}

	for _, pod := range kvpods.Items {
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		req := client.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{})
		podLogs, err := req.Stream(intCtx)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("reading request stream: %w", err)
		}
		defer podLogs.Close()

		buf := new(bytes.Buffer)
		_, err = io.Copy(buf, podLogs)
		if err != nil {
			return fmt.Errorf("failed to copy logs to buffer: %w", err)
		}

		path := filepath.Join(tempDirPath, fmt.Sprintf("%s-%s.log", pod.Namespace, pod.Name))
		err = os.WriteFile(path, buf.Bytes(), 0600)
		if err != nil {
			return fmt.Errorf("failed to write the log file %q: %w", path, err)
		}
	}

	return nil
}
