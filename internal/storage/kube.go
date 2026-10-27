package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Object is an unstructured Kubernetes object.
type Object = map[string]any

// Kube is the cluster access the controller needs.
type Kube interface {
	List(ctx context.Context, resource string) ([]Object, error)
	Apply(ctx context.Context, obj Object) error
	Delete(ctx context.Context, resource, namespace, name string) error
	SetStatus(ctx context.Context, resource, namespace, name string, status Object) error
}

// KubectlKube talks to the local K3s API with the node's own administrator
// credential, the same way the guest agent's other controllers do.
type KubectlKube struct{ Kubeconfig string }

func (k KubectlKube) kubectl(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "k3s", append([]string{"kubectl", "--kubeconfig=" + k.Kubeconfig}, args...)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

func (k KubectlKube) List(ctx context.Context, resource string) ([]Object, error) {
	b, err := k.kubectl(ctx, nil, "get", resource, "-A", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []Object `json:"items"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (k KubectlKube) Apply(ctx context.Context, obj Object) error {
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	_, err = k.kubectl(ctx, b, "apply", "-f", "-")
	return err
}

func (k KubectlKube) Delete(ctx context.Context, resource, namespace, name string) error {
	args := []string{"delete", resource, name, "--ignore-not-found", "--wait=false"}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	_, err := k.kubectl(ctx, nil, args...)
	return err
}

func (k KubectlKube) SetStatus(ctx context.Context, resource, namespace, name string, status Object) error {
	b, err := json.Marshal(Object{"status": status})
	if err != nil {
		return err
	}
	_, err = k.kubectl(ctx, nil, "patch", resource, name, "-n", namespace, "--subresource=status", "--type=merge", "-p", string(b))
	return err
}
