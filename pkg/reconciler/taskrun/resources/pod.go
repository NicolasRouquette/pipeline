/*
Copyright 2019 The Tekton Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package resources provides methods to convert a Build CRD to a k8s Pod
// resource.
package resources

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tektoncd/pipeline/pkg/apis/pipeline"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1alpha1"
	"github.com/tektoncd/pipeline/pkg/credentials"
	"github.com/tektoncd/pipeline/pkg/credentials/dockercreds"
	"github.com/tektoncd/pipeline/pkg/credentials/gitcreds"
	"github.com/tektoncd/pipeline/pkg/names"
	"github.com/tektoncd/pipeline/pkg/reconciler/taskrun/entrypoint"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
)

const (
	workspaceDir = "/workspace"
	homeDir      = "/builder/home"

	taskRunLabelKey     = pipeline.GroupName + pipeline.TaskRunLabelKey
	ManagedByLabelKey   = "app.kubernetes.io/managed-by"
	ManagedByLabelValue = "tekton-pipelines"

	scriptsDir = "/builder/scripts"
)

// These are effectively const, but Go doesn't have such an annotation.
var (
	groupVersionKind = schema.GroupVersionKind{
		Group:   v1alpha1.SchemeGroupVersion.Group,
		Version: v1alpha1.SchemeGroupVersion.Version,
		Kind:    "TaskRun",
	}
	emptyVolumeSource = corev1.VolumeSource{
		EmptyDir: &corev1.EmptyDirVolumeSource{},
	}
	// These are injected into all of the source/step containers.
	implicitEnvVars = []corev1.EnvVar{{
		Name:  "HOME",
		Value: homeDir,
	}}
	implicitVolumeMounts = []corev1.VolumeMount{{
		Name:      "workspace",
		MountPath: workspaceDir,
	}, {
		Name:      "home",
		MountPath: homeDir,
	}}
	implicitVolumes = []corev1.Volume{{
		Name:         "workspace",
		VolumeSource: emptyVolumeSource,
	}, {
		Name:         "home",
		VolumeSource: emptyVolumeSource,
	}}

	zeroQty = resource.MustParse("0")

	// Random byte reader used for pod name generation.
	// var for testing.
	randReader = rand.Reader

	// Volume definition attached to Pods generated from TaskRuns that have
	// steps that specify a Script.
	scriptsVolume = corev1.Volume{
		Name:         "place-scripts",
		VolumeSource: emptyVolumeSource,
	}
	scriptsVolumeMount = corev1.VolumeMount{
		Name:      "place-scripts",
		MountPath: scriptsDir,
	}
)

const (
	// Prefixes to add to the name of the init containers.
	containerPrefix            = "step-"
	unnamedInitContainerPrefix = "step-unnamed-"
	// Name of the credential initialization container.
	credsInit = "credential-initializer"
	// Name of the working dir initialization container.
	workingDirInit       = "working-dir-initializer"
	ReadyAnnotation      = "tekton.dev/ready"
	readyAnnotationValue = "READY"
)

func makeCredentialInitializer(credsImage, serviceAccountName, namespace string, kubeclient kubernetes.Interface) (*v1alpha1.Step, []corev1.Volume, error) {
	if serviceAccountName == "" {
		serviceAccountName = "default"
	}

	sa, err := kubeclient.CoreV1().ServiceAccounts(namespace).Get(serviceAccountName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}

	builders := []credentials.Builder{dockercreds.NewBuilder(), gitcreds.NewBuilder()}

	// Collect the volume declarations, there mounts into the cred-init container, and the arguments to it.
	volumes := []corev1.Volume{}
	volumeMounts := implicitVolumeMounts
	args := []string{}
	for _, secretEntry := range sa.Secrets {
		secret, err := kubeclient.CoreV1().Secrets(namespace).Get(secretEntry.Name, metav1.GetOptions{})
		if err != nil {
			return nil, nil, err
		}

		matched := false
		for _, b := range builders {
			if sa := b.MatchingAnnotations(secret); len(sa) > 0 {
				matched = true
				args = append(args, sa...)
			}
		}

		if matched {
			name := names.SimpleNameGenerator.RestrictLengthWithRandomSuffix(fmt.Sprintf("secret-volume-%s", secret.Name))
			volumeMounts = append(volumeMounts, corev1.VolumeMount{
				Name:      name,
				MountPath: credentials.VolumeName(secret.Name),
			})
			volumes = append(volumes, corev1.Volume{
				Name: name,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: secret.Name,
					},
				},
			})
		}
	}

	return &v1alpha1.Step{Container: corev1.Container{
		Name:         names.SimpleNameGenerator.RestrictLengthWithRandomSuffix(containerPrefix + credsInit),
		Image:        credsImage,
		Command:      []string{"/ko-app/creds-init"},
		Args:         args,
		VolumeMounts: volumeMounts,
		Env:          implicitEnvVars,
		WorkingDir:   workspaceDir,
	}}, volumes, nil
}

func makeWorkingDirScript(workingDirs map[string]bool) string {
	script := ""
	var orderedDirs []string

	for wd := range workingDirs {
		if wd != "" {
			orderedDirs = append(orderedDirs, wd)
		}
	}
	sort.Strings(orderedDirs)

	for _, wd := range orderedDirs {
		p := filepath.Clean(wd)
		if rel, err := filepath.Rel(workspaceDir, p); err == nil && !strings.HasPrefix(rel, ".") {
			if script == "" {
				script = fmt.Sprintf("mkdir -p %s", p)
			} else {
				script = fmt.Sprintf("%s %s", script, p)
			}
		}
	}

	return script
}

func makeWorkingDirInitializer(shellImage string, steps []v1alpha1.Step) *v1alpha1.Step {
	workingDirs := make(map[string]bool)
	for _, step := range steps {
		workingDirs[step.WorkingDir] = true
	}

	if script := makeWorkingDirScript(workingDirs); script != "" {
		return &v1alpha1.Step{Container: corev1.Container{
			Name:         names.SimpleNameGenerator.RestrictLengthWithRandomSuffix(containerPrefix + workingDirInit),
			Image:        shellImage,
			Command:      []string{"sh"},
			Args:         []string{"-c", script},
			VolumeMounts: implicitVolumeMounts,
			Env:          implicitEnvVars,
			WorkingDir:   workspaceDir,
		}}
	}
	return nil
}

// GetPod returns the Pod for the given pod name
type GetPod func(string, metav1.GetOptions) (*corev1.Pod, error)

// TryGetPod fetches the TaskRun's pod, returning nil if it has not been created or it does not exist.
func TryGetPod(taskRunStatus v1alpha1.TaskRunStatus, gp GetPod) (*corev1.Pod, error) {
	if taskRunStatus.PodName == "" {
		return nil, nil
	}

	pod, err := gp(taskRunStatus.PodName, metav1.GetOptions{})
	if err == nil || errors.IsNotFound(err) {
		return pod, nil
	}

	return nil, err
}

// MakePod converts TaskRun and TaskSpec objects to a Pod which implements the taskrun specified
// by the supplied CRD.
func MakePod(images pipeline.Images, taskRun *v1alpha1.TaskRun, taskSpec v1alpha1.TaskSpec, kubeclient kubernetes.Interface) (*corev1.Pod, error) {
	cred, secrets, err := makeCredentialInitializer(images.CredsImage, taskRun.GetServiceAccountName(), taskRun.Namespace, kubeclient)
	if err != nil {
		return nil, err
	}
	initSteps := []v1alpha1.Step{*cred}
	var podSteps []v1alpha1.Step

	if workingDir := makeWorkingDirInitializer(images.ShellImage, taskSpec.Steps); workingDir != nil {
		initSteps = append(initSteps, *workingDir)
	}

	maxIndicesByResource := findMaxResourceRequest(taskSpec.Steps, corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage)

	placeScripts := false
	placeScriptsStep := v1alpha1.Step{Container: corev1.Container{
		Name:         names.SimpleNameGenerator.RestrictLengthWithRandomSuffix("place-scripts"),
		Image:        images.ShellImage,
		TTY:          true,
		Command:      []string{"sh"},
		Args:         []string{"-c", ""},
		VolumeMounts: []corev1.VolumeMount{scriptsVolumeMount},
	}}

	for i, s := range taskSpec.Steps {
		s.Env = append(implicitEnvVars, s.Env...)
		// TODO(mattmoor): Check that volumeMounts match volumes.

		// Add implicit volume mounts, unless the user has requested
		// their own volume mount at that path.
		requestedVolumeMounts := map[string]bool{}
		for _, vm := range s.VolumeMounts {
			requestedVolumeMounts[filepath.Clean(vm.MountPath)] = true
		}
		for _, imp := range implicitVolumeMounts {
			if !requestedVolumeMounts[filepath.Clean(imp.MountPath)] {
				s.VolumeMounts = append(s.VolumeMounts, imp)
			}
		}

		// If the step specifies a Script, generate and invoke an
		// executable script file containing each item in the script.
		if s.Script != "" {
			placeScripts = true
			// Append to the place-scripts script to place the
			// script file in a known location in the scripts volume.
			tmpFile := filepath.Join(scriptsDir, names.SimpleNameGenerator.RestrictLengthWithRandomSuffix(fmt.Sprintf("script-%d", i)))
			// heredoc is the "here document" placeholder string
			// used to cat script contents into the file. Typically
			// this is the string "EOF" but if this value were
			// "EOF" it would prevent users from including the
			// string "EOF" in their own scripts. Instead we
			// randomly generate a string to (hopefully) prevent
			// collisions.
			heredoc := names.SimpleNameGenerator.RestrictLengthWithRandomSuffix("script-heredoc-randomly-generated")
			// NOTE: quotes around the heredoc string are
			// important. Without them, ${}s in the file are
			// interpreted as env vars and likely end up replaced
			// with empty strings. See
			// https://stackoverflow.com/a/27921346
			placeScriptsStep.Args[1] += fmt.Sprintf(`tmpfile="%s"
touch ${tmpfile} && chmod +x ${tmpfile}
cat > ${tmpfile} << '%s'
%s
%s
`, tmpFile, heredoc, s.Script, heredoc)
			// The entrypoint redirecter has already run on this
			// step, so we just need to replace the image's
			// entrypoint (if any) with the script to run.
			// Validation prevents step args from being passed, but
			// just to be careful we'll replace any that survived
			// entrypoint redirection here.

			// TODO(jasonhall): It's confusing that entrypoint
			// redirection isn't done as part of MakePod, and the
			// interaction of these two modifications to container
			// args might be confusing to debug in the future.
			s.Args = append(s.Args, tmpFile)
			for i := 0; i < len(s.Args); i++ {
				if s.Args[i] == "-entrypoint" {
					s.Args = append(s.Args[:i+1], tmpFile)
				}
			}
			s.VolumeMounts = append(s.VolumeMounts, scriptsVolumeMount)
		}

		if s.WorkingDir == "" {
			s.WorkingDir = workspaceDir
		}
		if s.Name == "" {
			s.Name = fmt.Sprintf("%v%d", unnamedInitContainerPrefix, i)
		} else {
			s.Name = names.SimpleNameGenerator.RestrictLength(fmt.Sprintf("%v%v", containerPrefix, s.Name))
		}
		// use the container name to add the entrypoint binary as an
		// init container.
		if s.Name == names.SimpleNameGenerator.RestrictLength(fmt.Sprintf("%v%v", containerPrefix, entrypoint.InitContainerName)) {
			initSteps = append(initSteps, s)
		} else {
			zeroNonMaxResourceRequests(&s, i, maxIndicesByResource)
			podSteps = append(podSteps, s)
		}
	}

	// Add podTemplate Volumes to the explicitly declared use volumes
	volumes := append(taskSpec.Volumes, taskRun.Spec.PodTemplate.Volumes...)
	// Add our implicit volumes and any volumes needed for secrets to the explicitly
	// declared user volumes.
	volumes = append(volumes, implicitVolumes...)
	volumes = append(volumes, secrets...)

	// Add the volume shared to place a script file, if any step specified
	// a script.
	if placeScripts {
		volumes = append(volumes, scriptsVolume)
		initSteps = append(initSteps, placeScriptsStep)
	}

	if err := v1alpha1.ValidateVolumes(volumes); err != nil {
		return nil, err
	}

	// Generate a short random hex string.
	b, err := ioutil.ReadAll(io.LimitReader(randReader, 3))
	if err != nil {
		return nil, err
	}
	gibberish := hex.EncodeToString(b)

	mergedInitSteps, err := v1alpha1.MergeStepsWithStepTemplate(taskSpec.StepTemplate, initSteps)
	if err != nil {
		return nil, err
	}
	var mergedInitContainers []corev1.Container
	for _, s := range mergedInitSteps {
		mergedInitContainers = append(mergedInitContainers, s.Container)
	}
	mergedPodSteps, err := v1alpha1.MergeStepsWithStepTemplate(taskSpec.StepTemplate, podSteps)
	if err != nil {
		return nil, err
	}
	var mergedPodContainers []corev1.Container
	for _, s := range mergedPodSteps {
		mergedPodContainers = append(mergedPodContainers, s.Container)
	}
	if len(taskSpec.Sidecars) > 0 {
		mergedPodContainers = append(mergedPodContainers, taskSpec.Sidecars...)
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			// We execute the build's pod in the same namespace as where the build was
			// created so that it can access colocated resources.
			Namespace: taskRun.Namespace,
			// Generate a unique name based on the build's name.
			// Add a unique suffix to avoid confusion when a build
			// is deleted and re-created with the same name.
			// We don't use RestrictLengthWithRandomSuffix here because k8s fakes don't support it.
			Name: fmt.Sprintf("%s-pod-%s", taskRun.Name, gibberish),
			// If our parent TaskRun is deleted, then we should be as well.
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(taskRun, groupVersionKind),
			},
			Annotations: makeAnnotations(taskRun),
			Labels:      makeLabels(taskRun),
		},
		Spec: corev1.PodSpec{
			RestartPolicy:      corev1.RestartPolicyNever,
			InitContainers:     mergedInitContainers,
			Containers:         mergedPodContainers,
			ServiceAccountName: taskRun.GetServiceAccountName(),
			Volumes:            volumes,
			NodeSelector:       taskRun.Spec.PodTemplate.NodeSelector,
			Tolerations:        taskRun.Spec.PodTemplate.Tolerations,
			Affinity:           taskRun.Spec.PodTemplate.Affinity,
			SecurityContext:    taskRun.Spec.PodTemplate.SecurityContext,
			RuntimeClassName:   taskRun.Spec.PodTemplate.RuntimeClassName,
		},
	}, nil
}

type UpdatePod func(*corev1.Pod) (*corev1.Pod, error)

// AddReadyAnnotation adds the ready annotation if it is not present.
// Returns any error that comes back from the passed-in update func.
func AddReadyAnnotation(p *corev1.Pod, update UpdatePod) error {
	if p.ObjectMeta.Annotations == nil {
		p.ObjectMeta.Annotations = make(map[string]string)
	}
	if p.ObjectMeta.Annotations[ReadyAnnotation] != readyAnnotationValue {
		p.ObjectMeta.Annotations[ReadyAnnotation] = readyAnnotationValue
		_, err := update(p)

		return err
	}

	return nil
}

func IsContainerStep(name string) bool {
	return strings.HasPrefix(name, containerPrefix)
}

// makeLabels constructs the labels we will propagate from TaskRuns to Pods.
func makeLabels(s *v1alpha1.TaskRun) map[string]string {
	labels := make(map[string]string, len(s.ObjectMeta.Labels)+1)
	// NB: Set this *before* passing through TaskRun labels. If the TaskRun
	// has a managed-by label, it should override this default.

	// Copy through the TaskRun's labels to the underlying Pod's.
	labels[ManagedByLabelKey] = ManagedByLabelValue
	for k, v := range s.ObjectMeta.Labels {
		labels[k] = v
	}

	// NB: Set this *after* passing through TaskRun Labels. If the TaskRun
	// specifies this label, it should be overridden by this value.
	labels[taskRunLabelKey] = s.Name
	return labels
}

// makeAnnotations constructs the annotations we will propagate from TaskRuns to Pods
// and adds any other annotations that will be needed to initialize a Pod.
func makeAnnotations(s *v1alpha1.TaskRun) map[string]string {
	annotations := make(map[string]string, len(s.ObjectMeta.Annotations)+1)
	for k, v := range s.ObjectMeta.Annotations {
		annotations[k] = v
	}
	annotations[ReadyAnnotation] = ""
	return annotations
}

// zeroNonMaxResourceRequests zeroes out the container's cpu, memory, or
// ephemeral storage resource requests if the container does not have the
// largest request out of all containers in the pod. This is done because Tekton
// overwrites each container's entrypoint to make containers effectively execute
// one at a time, so we want pods to only request the maximum resources needed
// at any single point in time. If no container has an explicit resource
// request, all requests are set to 0.
func zeroNonMaxResourceRequests(step *v1alpha1.Step, stepIndex int, maxIndicesByResource map[corev1.ResourceName]int) {
	if step.Resources.Requests == nil {
		step.Resources.Requests = corev1.ResourceList{}
	}
	for name, maxIdx := range maxIndicesByResource {
		if maxIdx != stepIndex {
			step.Resources.Requests[name] = zeroQty
		}
	}
}

// findMaxResourceRequest returns the index of the container with the maximum
// request for the given resource from among the given set of containers.
func findMaxResourceRequest(steps []v1alpha1.Step, resourceNames ...corev1.ResourceName) map[corev1.ResourceName]int {
	maxIdxs := make(map[corev1.ResourceName]int, len(resourceNames))
	maxReqs := make(map[corev1.ResourceName]resource.Quantity, len(resourceNames))
	for _, name := range resourceNames {
		maxIdxs[name] = -1
		maxReqs[name] = zeroQty
	}
	for i, s := range steps {
		for _, name := range resourceNames {
			maxReq := maxReqs[name]
			req, exists := s.Container.Resources.Requests[name]
			if exists && req.Cmp(maxReq) > 0 {
				maxIdxs[name] = i
				maxReqs[name] = req
			}
		}
	}
	return maxIdxs
}

// TrimContainerNamePrefix trim the container name prefix to get the corresponding step name
func TrimContainerNamePrefix(containerName string) string {
	return strings.TrimPrefix(containerName, containerPrefix)
}
