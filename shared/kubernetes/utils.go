// SPDX-FileCopyrightText: 2024 SUSE LLC
//
// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/uyuni-project/uyuni-tools/shared/types"
	"github.com/uyuni-project/uyuni-tools/shared/utils"
)

// ServerFilter represents filter used to check server app.
const ServerFilter = "-lapp=uyuni"

// ServerFilter represents filter used to check proxy app.
const ProxyFilter = "-lapp=uyuni-proxy"

// PgsqlRequiredVolumeMounts represents volumes mount used by PostgreSQL.
var PgsqlRequiredVolumeMounts = []types.VolumeMount{
	{MountPath: "/etc/pki/tls", Name: "etc-tls"},
	{MountPath: "/var/lib/pgsql", Name: "var-pgsql"},
	{MountPath: "/etc/rhn", Name: "etc-rhn"},
	{MountPath: "/etc/pki/spacewalk-tls", Name: "tls-key"},
}

// PgsqlRequiredVolumes represents volumes used by PostgreSQL.
var PgsqlRequiredVolumes = []types.Volume{
	{Name: "etc-tls", PersistentVolumeClaim: &types.PersistentVolumeClaim{ClaimName: "etc-tls"}},
	{Name: "var-pgsql", PersistentVolumeClaim: &types.PersistentVolumeClaim{ClaimName: "var-pgsql"}},
	{Name: "etc-rhn", PersistentVolumeClaim: &types.PersistentVolumeClaim{ClaimName: "etc-rhn"}},
	{Name: "tls-key",
		Secret: &types.Secret{
			SecretName: "uyuni-cert", Items: []types.SecretItem{
				{Key: "tls.crt", Path: "spacewalk.crt"},
				{Key: "tls.key", Path: "spacewalk.key"},
			},
		},
	},
}

// waitForDeployment waits at most 60s for a kubernetes deployment to have at least one replica.
// See [isDeploymentReady] for more details.
func WaitForDeployment(namespace string, name string, appName string) error {
	// Find the name of a replica pod
	// Using the app label is a shortcut, not the 100% acurate way to get from deployment to pod
	podName := ""
	jsonpath := fmt.Sprintf("jsonpath={.items[?(@.metadata.labels.app==\"%s\")].metadata.name}", appName)
	cmdArgs := []string{"get", "pod", "-o", jsonpath}
	cmdArgs = addNamespace(cmdArgs, namespace)

	for i := 0; i < 60; i++ {
		out, err := utils.RunCmdOutput(zerolog.DebugLevel, "kubectl", cmdArgs...)
		if err == nil {
			podName = string(out)
			break
		}
	}

	// We need to wait for the image to be pulled as this can add quite some time
	// Setting a timeout on this is very hard since it hightly depends on network speed and image size
	// List the Pulled events from the pod as we may not see the Pulling if the image was already downloaded
	err := WaitForPulledImage(namespace, podName)
	if err != nil {
		return fmt.Errorf("failed to pulled image: %s", err)
	}

	log.Info().Msgf("Waiting for %s deployment to be ready in %s namespace\n", name, namespace)
	// Wait for a replica to be ready
	for i := 0; i < 60; i++ {
		// TODO Look for pod failures
		if IsDeploymentReady(namespace, name) {
			return nil
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("failed to find a ready replica for deployment %s in namespace %s after 60s", name, namespace)
}

// WaitForPulledImage wait that image is pulled.
func WaitForPulledImage(namespace string, podName string) error {
	log.Info().Msgf("Waiting for image of %s pod in %s namespace to be pulled", podName, namespace)
	pulledArgs := []string{"get", "event",
		"-o", "jsonpath={.items[?(@.reason==\"Pulled\")].message}",
		"--field-selector", "involvedObject.name=" + podName}
	pulledArgs = addNamespace(pulledArgs, namespace)
	failedArgs := []string{"get", "event",
		"-o", "jsonpath={range .items[?(@.reason==\"Failed\")]}{.message}{\"\\n\"}{end}",
		"--field-selector", "involvedObject.name=" + podName}
	failedArgs = addNamespace(failedArgs, namespace)
	for {
		// Look for events indicating an image pull issue
		out, err := utils.RunCmdOutput(zerolog.TraceLevel, "kubectl", failedArgs...)
		if err != nil {
			return fmt.Errorf("failed to get failed events for pod %s", podName)
		}
		lines := strings.Split(string(out), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "Failed to pull image") {
				return errors.New("failed to pull image")
			}
		}

		// Has the image pull finished?
		out, err = utils.RunCmdOutput(zerolog.TraceLevel, "kubectl", pulledArgs...)
		if err != nil {
			return fmt.Errorf("failed to get events for pod %s", podName)
		}
		if len(out) > 0 {
			break
		}
		time.Sleep(1 * time.Second)
	}
	return nil
}

// IsDeploymentReady returns true if a kubernetes deployment has at least one ready replica.
// The name can also be a filter parameter like -lapp=uyuni.
// An empty namespace means searching through all the namespaces.
func IsDeploymentReady(namespace string, name string) bool {
	jsonpath := fmt.Sprintf("jsonpath={.items[?(@.metadata.name==\"%s\")].status.readyReplicas}", name)
	args := []string{"get", "-o", jsonpath, "deploy"}
	args = addNamespace(args, namespace)

	out, err := utils.RunCmdOutput(zerolog.TraceLevel, "kubectl", args...)
	// kubectl errors out if the deployment or namespace doesn't exist
	if err == nil {
		if replicas, _ := strconv.Atoi(string(out)); replicas > 0 {
			return true
		}
	}
	return false
}

// ReplicasTo set the replica for an app to the given value.
// Scale the number of replicas of the server.
func ReplicasTo(filter string, replica uint) error {
	args := []string{"scale", "deploy", "uyuni", "--replicas"}
	log.Debug().Msgf("Setting replicas for pod in %s to %d", filter, replica)
	args = append(args, fmt.Sprint(replica))

	_, err := utils.RunCmdOutput(zerolog.DebugLevel, "kubectl", args...)
	if err != nil {
		return fmt.Errorf("cannot run kubectl %s: %s", args, err)
	}

	pods, err := getPods(ServerFilter)
	if err != nil {
		return fmt.Errorf("cannot get pods for %s: %s", filter, err)
	}

	for _, pod := range pods {
		err = waitForReplica(pod, replica)
		if replica > 0 {
			return nil
		}
		if err != nil {
			return fmt.Errorf("replica to %d failed: %s", replica, err)
		}
	}

	log.Debug().Msgf("Replicas for pod in %s are now %d", filter, replica)

	return err
}

func getPods(filter string) (pods []string, err error) {
	log.Debug().Msgf("Checking all pods for %s", filter)
	cmdArgs := []string{"get", "pods", filter, "--output=custom-columns=:.metadata.name", "--no-headers"}
	out, err := utils.RunCmdOutput(zerolog.DebugLevel, "kubectl", cmdArgs...)
	if err != nil {
		return pods, fmt.Errorf("cannot execute %s: %s", strings.Join(cmdArgs, string(" ")), err)
	}
	lines := strings.Split(string(out), "\n")
	pods = append(pods, lines...)
	log.Debug().Msgf("Pods in %s are %s", filter, pods)

	return pods, err
}

func waitForReplica(podname string, replica uint) error {
	waitSeconds := 120
	log.Debug().Msgf("Checking replica for %s ready to %d", podname, replica)
	cmdArgs := []string{"get", "rs", podname, "--output=custom-columns=DESIRED:.status.replicas", "--no-headers"}

	var err error
	for i := 0; i < waitSeconds; i++ {
		out, err := utils.RunCmdOutput(zerolog.DebugLevel, "kubectl", cmdArgs...)
		outStr := strings.TrimSuffix(string(out), "\n")
		if err != nil {
			return fmt.Errorf("cannot execute %s: %s", strings.Join(cmdArgs, string(" ")), err)
		}
		if string(outStr) == fmt.Sprint(replica) {
			log.Debug().Msgf("%s pod replica is now %d", podname, replica)
			break
		}
		log.Debug().Msgf("Pod %s replica is %s in %d seconds.", podname, string(out), i)
		time.Sleep(1 * time.Second)
	}
	if err != nil {
		return fmt.Errorf("pod %s replica is not %d in %s seconds: %s", podname, replica, strconv.Itoa(waitSeconds), err)
	}
	return nil
}

func addNamespace(args []string, namespace string) []string {
	if namespace != "" {
		args = append(args, "-n", namespace)
	} else {
		args = append(args, "-A")
	}
	return args
}

// GetPullPolicy return pullpolicy in lower case, if exists.
func GetPullPolicy(name string) string {
	policies := map[string]string{
		"always":       "Always",
		"never":        "Never",
		"ifnotpresent": "IfNotPresent",
	}
	policy := policies[strings.ToLower(name)]
	if policy == "" {
		log.Fatal().Msgf("%s is not a valid image pull policy value", name)
	}
	return policy
}

// RunPod runs a pod, waiting for its execution and deleting it.
func RunPod(podname string, image string, pullPolicy string, command string, override ...string) error {
	arguments := []string{"run", podname, "--image", image, "--image-pull-policy", pullPolicy}

	if len(override) > 0 {
		arguments = append(arguments, `--override-type=strategic`)
		for _, arg := range override {
			overrideParam := "--overrides=" + arg
			arguments = append(arguments, overrideParam)
		}
	}

	arguments = append(arguments, "--command", "--", command)
	err := utils.RunCmdStdMapping("kubectl", arguments...)
	if err != nil {
		return fmt.Errorf("cannot run %s using image %s: %s", command, image, err)
	}
	err = waitForPod(podname)
	if err != nil {
		return fmt.Errorf("deleting pod %s. Status fails with error %s", podname, err)
	}

	defer func() {
		_, err = DeletePod(podname)
	}()
	return nil
}

// Delete a kubernetes pod named podname.
func DeletePod(podname string) (string, error) {
	arguments := []string{"delete", "pod", podname}
	out, err := utils.RunCmdOutput(zerolog.DebugLevel, "kubectl", arguments...)
	if err != nil {
		return "", fmt.Errorf("cannot delete pod %s: %s", podname, err)
	}
	return string(out), nil
}

func waitForPod(podname string) error {
	status := "Succeeded"
	waitSeconds := 120
	log.Debug().Msgf("Checking status for %s pod. Waiting %s seconds until status is %s", podname, strconv.Itoa(waitSeconds), status)
	cmdArgs := []string{"get", "pod", podname, "--output=custom-columns=STATUS:.status.phase", "--no-headers"}
	var err error
	for i := 0; i < waitSeconds; i++ {
		out, err := utils.RunCmdOutput(zerolog.DebugLevel, "kubectl", cmdArgs...)
		outStr := strings.TrimSuffix(string(out), "\n")
		if err != nil {
			return fmt.Errorf("cannot execute %s: %s", strings.Join(cmdArgs, string(" ")), err)
		}
		if strings.ToUpper(outStr) == strings.ToUpper(status) {
			log.Debug().Msgf("%s pod status is %s", podname, status)
			return nil
		}
		log.Debug().Msgf("Pod %s status is %s in %d seconds.", podname, string(out), i)
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("pod %s status is not %s in %s seconds: %s", podname, status, strconv.Itoa(waitSeconds), err)
}

// GetNode return the node where the app is running.
func GetNode(appName string) (string, error) {
	nodeName := ""
	cmdArgs := []string{"get", "pod", "-lapp=" + appName, "-o", "jsonpath={.items[*].spec.nodeName}"}
	for i := 0; i < 60; i++ {
		out, err := utils.RunCmdOutput(zerolog.DebugLevel, "kubectl", cmdArgs...)
		if err == nil {
			nodeName = string(out)
			break
		}
	}
	if len(nodeName) > 0 {
		log.Debug().Msgf("Node name for app %s is: %s", appName, nodeName)
	} else {
		return "", fmt.Errorf("cannot find Node name for app %s", appName)
	}
	return nodeName, nil
}

// GenerateOverrideDeployment generate a JSON files represents the deployment information.
func GenerateOverrideDeployment(deployData types.Deployment) (string, error) {
	ret, err := json.Marshal(deployData)
	if err != nil {
		return "", fmt.Errorf("cannot marshal deployment %s", err)
	}
	return string(ret), nil
}
