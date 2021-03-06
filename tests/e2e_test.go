package tests

import (
	"fmt"
	"io/ioutil"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mirantis/sriov-scheduler/pkg/extender"

	"flag"

	"github.com/ghodss/yaml"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	apps "k8s.io/client-go/pkg/apis/apps/v1beta1"
	"k8s.io/client-go/pkg/apis/extensions/v1beta1"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	vfsDevice       = "eth3"
	policyConfigMap = "scheduler-policy"
	sriovTotalVFs   = "1"
)

var (
	kubeconfig          string
	deploymentDirectory string
	master              string
)

func init() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Kubernetes config")
	flag.StringVar(&deploymentDirectory, "deployments", "", "Directory with all deployment definitions")
	flag.StringVar(&master, "master", "kube-master", "Name opf the kubernetes master node")
}

// TestSriovExtender runs next scenario:
// 1. Read discovery and extender definitions from this repository tools directory
// 2. Patch discovery daemonset with fake sriov_totalvfs mount
// 3. Deploy discovery daemonset and wait until totalvfs resource will be saved on nodes
// 4. Deployment extender deployment and service. Wait until extender pods are ready.
// 5. Update policy config for scheduler.
// 6. Create 2 pods that require sriov network.
// 7. Verify that 2 pods will be running and in ready state.
func TestSriovExtender(t *testing.T) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	require.NoError(t, err)
	client, err := kubernetes.NewForConfig(config)
	require.NoError(t, err)

	discovery := v1beta1.DaemonSet{}
	discoveryData, err := ioutil.ReadFile(filepath.Join(deploymentDirectory, "discovery.yaml"))
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(discoveryData, &discovery))

	extend := &apps.Deployment{}
	extenderSvc := v1.Service{}
	extenderDataMulti, err := ioutil.ReadFile(filepath.Join(deploymentDirectory, "extender.yaml"))
	require.NoError(t, err)
	extenderData := strings.Split(string(extenderDataMulti), "---\n")
	require.Len(t, extenderData, 2)
	require.NoError(t, yaml.Unmarshal([]byte(extenderData[0]), &extenderSvc))
	require.NoError(t, yaml.Unmarshal([]byte(extenderData[1]), extend))

	sriovTotalVFsQuantity, err := resource.ParseQuantity(sriovTotalVFs)
	require.NoError(t, err)
	fakeVFs := v1.ConfigMap{
		ObjectMeta: meta_v1.ObjectMeta{
			Name:      "faketotalvfs",
			Namespace: discovery.Namespace,
		},
		Data: map[string]string{"sriov_totalvfs": sriovTotalVFs},
	}
	_, err = client.ConfigMaps(fakeVFs.Namespace).Create(&fakeVFs)
	require.NoError(t, err)

	require.Len(t, discovery.Spec.Template.Spec.Volumes, 1)
	discovery.Spec.Template.Spec.Volumes[0] = v1.Volume{
		Name: "sys",
		VolumeSource: v1.VolumeSource{
			ConfigMap: &v1.ConfigMapVolumeSource{LocalObjectReference: v1.LocalObjectReference{Name: fakeVFs.Name}},
		},
	}
	require.Len(t, discovery.Spec.Template.Spec.Containers, 1)
	require.Len(t, discovery.Spec.Template.Spec.Containers[0].VolumeMounts, 1)
	discovery.Spec.Template.Spec.Containers[0].VolumeMounts[0] = v1.VolumeMount{
		Name:      "sys",
		MountPath: fmt.Sprintf("/test/sys/class/net/%s/device/", vfsDevice),
	}
	discovery.Spec.Template.Spec.Containers[0].Command = append(
		discovery.Spec.Template.Spec.Containers[0].Command, "--device", vfsDevice, "--directory", "/test")
	_, err = client.DaemonSets(discovery.Namespace).Create(&discovery)
	require.NoError(t, err)
	require.NoError(t, Eventually(func() error {
		nodes, err := client.Nodes().List(meta_v1.ListOptions{})
		if err != nil {
			return err
		}
		for _, node := range nodes.Items {
			if node.Name == master {
				continue
			}
			if val, exists := node.Status.Allocatable[extender.TotalVFsResource]; !exists {
				return fmt.Errorf("node %s doesnt have totalvfs discovered", node.Name)
			} else if val.Cmp(sriovTotalVFsQuantity) != 0 {
				return fmt.Errorf(
					"discovered quantity %v is different from expected %v on node %s",
					&val, &sriovTotalVFsQuantity, node.Name,
				)
			}

		}
		return nil
	}, 10*time.Second, 500*time.Millisecond))

	extend, err = client.AppsV1beta1().Deployments(extend.Namespace).Create(extend)
	require.NoError(t, err)
	_, err = client.Services(extenderSvc.Namespace).Create(&extenderSvc)
	require.NoError(t, err)
	require.NoError(t, Eventually(func() error {
		pods, err := client.Core().Pods(extend.Namespace).List(
			meta_v1.ListOptions{
				LabelSelector: labels.Set(extend.Spec.Selector.MatchLabels).String(),
			})
		if err != nil {
			return err
		}
		if lth := int32(len(pods.Items)); lth != *extend.Spec.Replicas {
			return fmt.Errorf("unexpected number of replices %d != %d for extender", lth, extend.Spec.Replicas)
		}
		for _, pod := range pods.Items {
			if pod.Status.Phase != v1.PodRunning {
				return fmt.Errorf("pod %v is not yet running", &pod)
			}
		}
		return nil
	}, 10*time.Second, 500*time.Millisecond))

	schedulerPod, err := client.Core().Pods("kube-system").Get("kube-scheduler-kube-master", meta_v1.GetOptions{})

	cmd := exec.Command("docker", "exec", master, "mv", "/etc/kubernetes/manifests/kube-scheduler.yaml", "/tmp")
	require.NoError(t, cmd.Run())
	require.NoError(t, Eventually(func() error {
		_, err := client.Core().Pods("kube-system").Get("kube-scheduler-kube-master", meta_v1.GetOptions{})
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("expected that object will be not found")
	}, 10*time.Second, 500*time.Millisecond))
	require.Len(t, schedulerPod.Spec.Containers, 1)
	schedulerPod.Spec.Containers[0].Command = append(
		schedulerPod.Spec.Containers[0].Command,
		"--policy-configmap", policyConfigMap,
	)
	newSchedulerPod := &v1.Pod{
		ObjectMeta: meta_v1.ObjectMeta{
			Name:      "sriov-test-" + schedulerPod.Name,
			Namespace: "kube-system",
		},
		Spec: schedulerPod.Spec,
	}

	policyData, err := ioutil.ReadFile(filepath.Join(deploymentDirectory, "scheduler.json"))
	require.NoError(t, err)

	policyCfg := v1.ConfigMap{
		ObjectMeta: meta_v1.ObjectMeta{
			Name:      policyConfigMap,
			Namespace: newSchedulerPod.Namespace,
		},
		Data: map[string]string{"policy.cfg": string(policyData)},
	}
	_, err = client.ConfigMaps(policyCfg.Namespace).Create(&policyCfg)
	require.NoError(t, err)
	t.Logf("creating pod %v", newSchedulerPod)
	newSchedulerPod, err = client.Core().Pods(newSchedulerPod.Namespace).Create(newSchedulerPod)
	require.NoError(t, err)
	require.NoError(t, Eventually(func() error {
		pod, err := client.Core().Pods(newSchedulerPod.Namespace).Get(newSchedulerPod.Name, meta_v1.GetOptions{})
		if err != nil {
			return err
		}
		if pod.Status.Phase != v1.PodRunning {
			return fmt.Errorf("scheduler is not running %s", pod)
		}
		return nil
	}, 10*time.Second, 500*time.Millisecond))

	var sriovPods int32 = 3
	sriovDeployment := &apps.Deployment{
		ObjectMeta: meta_v1.ObjectMeta{
			Name:      "sriov-test-deployment",
			Namespace: "default",
		},
		Spec: apps.DeploymentSpec{
			Replicas: &sriovPods,
			Template: v1.PodTemplateSpec{
				ObjectMeta: meta_v1.ObjectMeta{
					Labels:      map[string]string{"app": "sriov-test-deployment"},
					Annotations: map[string]string{"networks": "sriov"},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:  "test-pause-container",
							Image: "gcr.io/google_containers/pause:3.0",
						},
					},
				},
			},
		},
	}
	sriovDeployment, err = client.AppsV1beta1().Deployments(sriovDeployment.Namespace).Create(sriovDeployment)
	require.NoError(t, err)
	require.NoError(t, Eventually(func() error {
		pods, err := client.Core().Pods(sriovDeployment.Namespace).List(
			meta_v1.ListOptions{
				LabelSelector: labels.Set(sriovDeployment.Spec.Selector.MatchLabels).String(),
			})
		if err != nil {
			return err
		}
		if int32(len(pods.Items)) != *sriovDeployment.Spec.Replicas {
			return fmt.Errorf("some pods were not yet created for deployment %s", sriovDeployment.Name)
		}
		var running int
		var pending int
		for _, pod := range pods.Items {
			switch pod.Status.Phase {
			case v1.PodRunning:
				running++
			case v1.PodPending:
				pending++
			}
		}
		if running != 2 {
			return fmt.Errorf("unexpected number of running pods %d - %v", running, pods)
		}
		if pending != 1 {
			return fmt.Errorf("unexpected number of pending pods %d - %v", pending, pods)
		}
		return nil
	}, 10*time.Second, 500*time.Millisecond))
}

// TestControllerRestarted verifies that without syncing controller after restart -
// application will schedule more pods than available vfs
// 1. 4 vfs - 2 for each node
// 2. schedule 3 pods with vf
// 3. stop scheduler
// 4. schedule 3 more pods
// 5. start scheduler
// 6. validate that only 1 will be scheduled
func TestControllerRestarted(t *testing.T) {

}

// TODO replace with wait.PollUntil
func Eventually(f func() error, timeout, timeinterval time.Duration) error {
	ticker := time.NewTicker(timeinterval).C
	timer := time.NewTimer(timeout).C
	var err error
	for {
		select {
		case <-ticker:
			err = f()
			if err != nil {
				continue
			}
			return nil
		case <-timer:
			return err
		}
	}
}
