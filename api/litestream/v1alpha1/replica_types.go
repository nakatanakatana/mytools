package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ReplicaType identifies the storage backend represented by a ReplicaSpec.
type ReplicaType string

const (
	ReplicaTypeS3     ReplicaType = "s3"
	ReplicaTypeGCS    ReplicaType = "gcs"
	ReplicaTypeAzure  ReplicaType = "azure"
	ReplicaTypeFile   ReplicaType = "file"
	ReplicaTypeNATS   ReplicaType = "nats"
	ReplicaTypeOSS    ReplicaType = "oss"
	ReplicaTypeSFTP   ReplicaType = "sftp"
	ReplicaTypeWebDAV ReplicaType = "webdav"
)

// ReplicaSpec is a tagged union of all supported Litestream replica backends.
// Exactly one backend pointer must be set and must match Type.
type ReplicaSpec struct {
	// +kubebuilder:validation:Enum=s3;gcs;azure;file;nats;oss;sftp;webdav
	Type ReplicaType `json:"type"`

	S3     *S3ReplicaSpec     `json:"s3,omitempty"`
	GCS    *GCSReplicaSpec    `json:"gcs,omitempty"`
	Azure  *AzureReplicaSpec  `json:"azure,omitempty"`
	File   *FileReplicaSpec   `json:"file,omitempty"`
	NATS   *NATSReplicaSpec   `json:"nats,omitempty"`
	OSS    *OSSReplicaSpec    `json:"oss,omitempty"`
	SFTP   *SFTPReplicaSpec   `json:"sftp,omitempty"`
	WebDAV *WebDAVReplicaSpec `json:"webdav,omitempty"`
}

// SecretReference identifies a Secret value without exposing that value to the
// controller or webhook.
type SecretReference struct {
	SecretKeyRef corev1.SecretKeySelector `json:"secretKeyRef"`
}

// S3ReplicaSpec configures an S3-compatible replica.
type S3ReplicaSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^.*[^[:space:]].*$`
	Bucket string `json:"bucket"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^.*[^[:space:]].*$`
	Path   string `json:"path"`
	Region string `json:"region,omitempty"`

	Endpoint       string        `json:"endpoint,omitempty"`
	ForcePathStyle *bool         `json:"forcePathStyle,omitempty"`
	SkipVerify     *bool         `json:"skipVerify,omitempty"`
	Credentials    S3Credentials `json:"credentials,omitempty"`
}

// S3Credentials configures optional static S3 credentials.
type S3Credentials struct {
	AccessKeyID     *SecretReference `json:"accessKeyID,omitempty"`
	SecretAccessKey *SecretReference `json:"secretAccessKey,omitempty"`
}

// GCSReplicaSpec configures a Google Cloud Storage replica.
type GCSReplicaSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^.*[^[:space:]].*$`
	Bucket string `json:"bucket"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^.*[^[:space:]].*$`
	Path string `json:"path"`

	// ServiceAccountJSON is optional when workload identity supplies credentials.
	ServiceAccountJSON *SecretReference `json:"serviceAccountJSON,omitempty"`
}

// AzureReplicaSpec configures an Azure Blob Storage replica.
type AzureReplicaSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^.*[^[:space:]].*$`
	AccountName string `json:"accountName"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^.*[^[:space:]].*$`
	Container string `json:"container"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^.*[^[:space:]].*$`
	Path     string `json:"path"`
	Endpoint string `json:"endpoint,omitempty"`

	// AccountKey is optional when managed identity supplies credentials.
	AccountKey *SecretReference `json:"accountKey,omitempty"`
}

// FileReplicaSpec configures a filesystem replica.
type FileReplicaSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^.*[^[:space:]].*$`
	Path string `json:"path"`
}

// NATSReplicaSpec configures a NATS JetStream object-store replica.
type NATSReplicaSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^.*[^[:space:]].*$`
	URL string `json:"url"`

	Username *SecretReference `json:"username,omitempty"`
	Password *SecretReference `json:"password,omitempty"`
	JWT      *SecretReference `json:"jwt,omitempty"`
	Seed     *SecretReference `json:"seed,omitempty"`
	Creds    *SecretReference `json:"creds,omitempty"`
	NKey     *SecretReference `json:"nkey,omitempty"`
	Token    *SecretReference `json:"token,omitempty"`

	RootCAs    []SecretReference `json:"rootCAs,omitempty"`
	ClientCert *SecretReference  `json:"clientCert,omitempty"`
	ClientKey  *SecretReference  `json:"clientKey,omitempty"`

	MaxReconnects *int             `json:"maxReconnects,omitempty"`
	ReconnectWait *metav1.Duration `json:"reconnectWait,omitempty"`
	Timeout       *metav1.Duration `json:"timeout,omitempty"`
}

// OSSReplicaSpec configures an Alibaba Cloud OSS replica.
type OSSReplicaSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^.*[^[:space:]].*$`
	Bucket string `json:"bucket"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^.*[^[:space:]].*$`
	Path     string `json:"path"`
	Endpoint string `json:"endpoint,omitempty"`
	Region   string `json:"region,omitempty"`

	AccessKeyID     *SecretReference `json:"accessKeyID,omitempty"`
	AccessKeySecret *SecretReference `json:"accessKeySecret,omitempty"`
}

// SFTPReplicaSpec configures an SFTP replica.
type SFTPReplicaSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^.*[^[:space:]].*$`
	Host string `json:"host"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^.*[^[:space:]].*$`
	User string `json:"user"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^.*[^[:space:]].*$`
	Path string `json:"path"`

	Password         *SecretReference `json:"password,omitempty"`
	PrivateKey       *SecretReference `json:"privateKey,omitempty"`
	ConcurrentWrites *bool            `json:"concurrentWrites,omitempty"`
}

// WebDAVReplicaSpec configures a WebDAV replica.
type WebDAVReplicaSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^.*[^[:space:]].*$`
	URL  string `json:"url"`
	Path string `json:"path,omitempty"`

	Username   *SecretReference `json:"username,omitempty"`
	Password   *SecretReference `json:"password,omitempty"`
	SkipVerify *bool            `json:"skipVerify,omitempty"`
}
