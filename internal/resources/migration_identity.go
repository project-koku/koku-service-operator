package resources

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// MigrationExecutionAnnotation identifies schema execution independently of the
// legacy image-tag annotation used by existing lifecycle tools.
const MigrationExecutionAnnotation = "koku.costmanagement.io/migration-execution"

// MigrationInputRevisionAnnotation versions mounted seed data independently of
// the application image's tag. Changing a descriptive tag on a digest pin must
// not invalidate an otherwise identical migration.
const MigrationInputRevisionAnnotation = "koku.costmanagement.io/migration-input-revision"

type migrationContainerInput struct {
	Name         string                 `json:"name"`
	Image        string                 `json:"image"`
	Command      []string               `json:"command,omitempty"`
	Args         []string               `json:"args,omitempty"`
	WorkingDir   string                 `json:"workingDir,omitempty"`
	Env          []corev1.EnvVar        `json:"env,omitempty"`
	EnvFrom      []corev1.EnvFromSource `json:"envFrom,omitempty"`
	VolumeMounts []corev1.VolumeMount   `json:"volumeMounts,omitempty"`
}

type migrationVolumeInput struct {
	Name      string `json:"name"`
	ConfigMap string `json:"configMap,omitempty"`
	Secret    string `json:"secret,omitempty"`
}

// MigrationExecutionID fingerprints the migration image, command, connection
// references, and seed revision. It can also identify legacy Jobs that predate
// the annotation. The live Pod template is authoritative, not a copied receipt.
//
// Init-container images, resource sizing, retry budgets, and scheduling do not
// identify schema execution: changing the operator's wait-for image must not
// rerun a completed migration. Referenced Secret/ConfigMap contents are not read;
// mounted migration input changes must carry an explicit seed/script revision.
func MigrationExecutionID(job *batchv1.Job) string {
	input := struct {
		Version    string                    `json:"version"`
		Revision   string                    `json:"revision"`
		Containers []migrationContainerInput `json:"containers"`
		Volumes    []migrationVolumeInput    `json:"volumes,omitempty"`
	}{Version: "v1", Revision: job.Annotations[MigrationInputRevisionAnnotation]}
	if input.Revision == "" && job.Labels[labelComponent] == "rbac-migration" {
		// Legacy RBAC Jobs stored the seed revision as a suffix of the tag.
		tag := job.Annotations[MigrationImageTagAnnotation]
		if index := strings.LastIndex(tag, "-cmseed"); index >= 0 {
			input.Revision = tag[index+1:]
		}
	}
	for _, container := range job.Spec.Template.Spec.Containers {
		input.Containers = append(input.Containers, migrationContainerInput{
			Name: container.Name, Image: container.Image,
			Command: container.Command, Args: container.Args, WorkingDir: container.WorkingDir,
			Env: container.Env, EnvFrom: container.EnvFrom, VolumeMounts: container.VolumeMounts,
		})
	}
	for _, volume := range job.Spec.Template.Spec.Volumes {
		ref := migrationVolumeInput{Name: volume.Name}
		if volume.ConfigMap != nil {
			ref.ConfigMap = volume.ConfigMap.Name
		}
		if volume.Secret != nil {
			ref.Secret = volume.Secret.SecretName
		}
		if ref.ConfigMap != "" || ref.Secret != "" {
			input.Volumes = append(input.Volumes, ref)
		}
	}
	// The input contains only JSON-supported structs, strings, and slices.
	data, _ := json.Marshal(input)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
