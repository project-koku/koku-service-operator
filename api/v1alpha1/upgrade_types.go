package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	ConditionRecoveryRequired    = "RecoveryRequired"
	ImagePolicyPinned            = "Pinned"
	ImagePolicyFollowOperator    = "FollowOperator"
	RecoveryRetry                = "Retry"
	RecoveryResumePrevious       = "ResumePrevious"
	UpgradeStageQuiescing        = "Quiescing"
	UpgradeStageMigrating        = "Migrating"
	UpgradeStageRollingOut       = "RollingOut"
	UpgradeStageRollingBack      = "RollingBack"
	UpgradeStageRecoveryRequired = "RecoveryRequired"
	UpgradeStageRolledBack       = "RolledBack"
	UpgradeStageRecovering       = "Recovering"
)

// UpgradePolicy controls application image ownership and explicit recovery.
// PostgreSQL engine upgrades and restores remain customer-owned.
type UpgradePolicy struct {
	// Pinned requires explicit images. FollowOperator resolves omitted images
	// from the operator's embedded release bundle; complete explicit pins win.
	// +kubebuilder:validation:Enum=Pinned;FollowOperator
	// +kubebuilder:default:=Pinned
	ImagePolicy string `json:"imagePolicy,omitempty"`
	// TimeoutSeconds bounds an upgrade, including graceful worker shutdown.
	// Expiry never force-deletes migration Jobs or terminating application Pods.
	// +kubebuilder:default:=7200
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=86400
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
	// Recovery is an explicit request after investigating/repairing database state.
	Recovery *UpgradeRecoveryRequest `json:"recovery,omitempty"`
}

type UpgradeRecoveryRequest struct {
	// OperationID must identify the currently failed operation.
	// +kubebuilder:validation:MinLength=1
	OperationID string `json:"operationID"`
	// RequestID is a unique, administrator-chosen idempotency key.
	// +kubebuilder:validation:MinLength=1
	RequestID string `json:"requestID"`
	// Retry accepts the current requested target after database inspection.
	// ResumePrevious confirms the database was restored/repaired to support
	// the previous revision at its recorded connection references.
	// +kubebuilder:validation:Enum=Retry;ResumePrevious
	Action string `json:"action"`
}

// ApplicationRevision contains resolved images and configuration references.
// Secret objects are never read into this snapshot. It has the same access
// boundary as the CMSC spec, including user-supplied literal environment values.
type ApplicationRevision struct {
	ID               string            `json:"id"`
	ReleaseID        string            `json:"releaseID"`
	ReleaseManaged   bool              `json:"releaseManaged,omitempty"`
	Images           map[string]string `json:"images"`
	Migrations       map[string]string `json:"migrations"`
	DiscoveredConfig *DiscoveredConfig `json:"discoveredConfig,omitempty"`
	// Spec is a frozen, resolved CMSC spec, allowing restart-safe execution and
	// recovery without rewriting the user's requested spec or explicit pins.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	Spec runtime.RawExtension `json:"spec"`
}

type UpgradeOperation struct {
	ID               string               `json:"id"`
	TargetGeneration int64                `json:"targetGeneration"`
	Source           *ApplicationRevision `json:"source,omitempty"`
	Target           ApplicationRevision  `json:"target"`
	// +kubebuilder:validation:Enum=Quiescing;Migrating;RollingOut;RollingBack;RecoveryRequired;RolledBack;Recovering
	Stage          string      `json:"stage"`
	StartedAt      metav1.Time `json:"startedAt"`
	StageStartedAt metav1.Time `json:"stageStartedAt"`
	// SchemaMutationPossible is persisted before starting a migration. Once
	// true, automatic image restoration must not resume old application writers.
	SchemaMutationPossible bool   `json:"schemaMutationPossible,omitempty"`
	Failure                string `json:"failure,omitempty"`
	RecoveryRequestID      string `json:"recoveryRequestID,omitempty"`
}

type UpgradeStatus struct {
	Installed *ApplicationRevision `json:"installed,omitempty"`
	Operation *UpgradeOperation    `json:"operation,omitempty"`
}
