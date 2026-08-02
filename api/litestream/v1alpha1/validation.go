package v1alpha1

import (
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

var permissionModePattern = regexp.MustCompile(`^0[0-7]{3}$`)
var imageDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var imageTagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
var imageRepositoryComponentPattern = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
var imageRegistryComponentPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
var txIDPattern = regexp.MustCompile(`^[0-9a-fA-F]+$`)

const maxDatabaseNameLength = 44

// Validate checks that fields whose validity depends on other fields are
// consistent.
func (s *LitestreamSpec) Validate() field.ErrorList {
	var errors field.ErrorList
	specPath := field.NewPath("spec")

	if len(s.Databases) == 0 {
		errors = append(errors, field.Required(specPath.Child("databases"), "at least one database is required"))
	}
	errors = append(errors, validateImage(s.Image, specPath.Child("image"))...)

	names := make(map[string]struct{}, len(s.Databases))
	paths := make(map[string]struct{}, len(s.Databases))
	for i := range s.Databases {
		databasePath := specPath.Child("databases").Index(i)
		errors = append(errors, s.Databases[i].validate(databasePath, names, paths)...)
	}

	errors = append(errors, validatePermissionMode(
		s.Injection.Permissions.DirectoryMode,
		specPath.Child("injection", "permissions", "directoryMode"),
	)...)
	errors = append(errors, validatePermissionMode(
		s.Injection.Permissions.DatabaseMode,
		specPath.Child("injection", "permissions", "databaseMode"),
	)...)
	return errors
}

func validateImage(image ImageSpec, path *field.Path) field.ErrorList {
	if image.Repository == "" && image.Tag == "" && image.Digest == "" {
		return nil
	}

	var errors field.ErrorList
	if image.Digest == "" {
		errors = append(errors, field.Invalid(path, image, "image overrides must include an immutable sha256 digest"))
	} else if !imageDigestPattern.MatchString(image.Digest) {
		errors = append(errors, field.Invalid(path.Child("digest"), image.Digest, "must be a sha256 digest with 64 lowercase hexadecimal characters"))
	}
	if image.Tag != "" {
		if image.Digest == "" {
			errors = append(errors, field.Forbidden(path.Child("tag"), "a tag requires an immutable sha256 digest"))
		} else if !imageTagPattern.MatchString(image.Tag) {
			errors = append(errors, field.Invalid(path.Child("tag"), image.Tag, "must be a valid image tag"))
		}
	}
	if strings.Contains(image.Repository, "@") {
		errors = append(errors, field.Invalid(path.Child("repository"), image.Repository, "repository must not include a digest"))
	} else if repositoryHasTag(image.Repository) {
		errors = append(errors, field.Invalid(path.Child("repository"), image.Repository, "repository must not include a tag; use digest"))
	} else if err := ValidateImageRepository(image.Repository); err != nil {
		errors = append(errors, field.Invalid(path.Child("repository"), image.Repository, "must be a valid container image repository"))
	}
	return errors
}

// ValidateImageRepository validates a repository without accepting a tag or
// digest. It is shared by the API validation and Pod webhook image resolver so
// controller defaults and resource overrides use the same syntax rules.
func ValidateImageRepository(repository string) error {
	if repository == "" {
		return nil
	}
	if len(repository) > 255 {
		return fmt.Errorf("repository exceeds 255 characters")
	}
	parts := strings.Split(repository, "/")
	if len(parts) == 0 {
		return fmt.Errorf("repository is empty")
	}

	componentStart := 0
	if len(parts) > 1 && isImageRegistry(parts[0]) {
		if err := validateImageRegistry(parts[0]); err != nil {
			return err
		}
		componentStart = 1
	}
	if componentStart == len(parts) {
		return fmt.Errorf("repository name is missing")
	}
	for _, component := range parts[componentStart:] {
		if !imageRepositoryComponentPattern.MatchString(component) {
			return fmt.Errorf("repository component %q is invalid", component)
		}
	}
	return nil
}

func isImageRegistry(first string) bool {
	return first == "localhost" || strings.Contains(first, ".") || strings.Contains(first, ":")
}

func validateImageRegistry(registry string) error {
	host := registry
	if separator := strings.LastIndexByte(registry, ':'); separator >= 0 {
		port := registry[separator+1:]
		if port == "" {
			return fmt.Errorf("registry port is missing")
		}
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil || value == 0 {
			return fmt.Errorf("registry port %q is invalid", port)
		}
		host = registry[:separator]
	}
	if host == "localhost" || net.ParseIP(host) != nil {
		return nil
	}
	for _, component := range strings.Split(host, ".") {
		if !imageRegistryComponentPattern.MatchString(component) {
			return fmt.Errorf("registry host %q is invalid", host)
		}
	}
	return nil
}

func repositoryHasTag(repository string) bool {
	return strings.LastIndex(repository, ":") > strings.LastIndex(repository, "/")
}

// Validate checks the replica endpoint's tagged-union backend configuration.
func (s *LitestreamReplicaSpec) Validate() field.ErrorList {
	return s.Replica.Validate(field.NewPath("spec", "replica"))
}

func (d *DatabaseBinding) validate(path *field.Path, names, paths map[string]struct{}) field.ErrorList {
	var errors field.ErrorList

	if d.Name == "" {
		errors = append(errors, field.Required(path.Child("name"), "name is required"))
	} else {
		namePath := path.Child("name")
		if len(d.Name) > maxDatabaseNameLength {
			errors = append(errors, field.Invalid(namePath, d.Name, "must be at most 44 characters because it forms an init container name"))
		}
		if problems := validation.IsDNS1123Label(d.Name); len(problems) > 0 {
			errors = append(errors, field.Invalid(namePath, d.Name, "must be a lowercase DNS label: "+strings.Join(problems, "; ")))
		}
		if _, ok := names[d.Name]; ok {
			errors = append(errors, field.Duplicate(namePath, d.Name))
		} else {
			names[d.Name] = struct{}{}
		}
	}

	cleanPath := filepath.Clean(d.Path)
	if d.Path == "" || !filepath.IsAbs(d.Path) || cleanPath != d.Path {
		errors = append(errors, field.Invalid(path.Child("path"), d.Path, "must be a non-empty absolute clean path"))
	} else if _, ok := paths[cleanPath]; ok {
		errors = append(errors, field.Duplicate(path.Child("path"), d.Path))
	} else {
		paths[cleanPath] = struct{}{}
	}

	if d.Restore == nil && d.Replicate == nil {
		errors = append(errors, field.Required(path, "restore or replicate is required"))
	}
	if d.ClonePolicy != "" && (d.Restore == nil || d.Replicate == nil) {
		errors = append(errors, field.Required(path.Child("restore"), "restore and replicate are required when clonePolicy is set"))
	}
	if d.Restore != nil && d.Replicate != nil && d.Restore.ReplicaRef.Name == d.Replicate.ReplicaRef.Name && d.ClonePolicy != "" {
		errors = append(errors, field.Forbidden(path.Child("clonePolicy"), "must be empty when restore and replicate reference the same replica"))
	}

	if d.Restore != nil {
		restorePath := path.Child("restore")
		errors = append(errors, validateLocalObjectReference(d.Restore.ReplicaRef, restorePath.Child("replicaRef"))...)
		if d.Restore.Timestamp != "" && d.Restore.TxID != "" {
			errors = append(errors, field.Forbidden(restorePath, "timestamp and txid are mutually exclusive"))
		}
		if d.Restore.Timestamp != "" {
			if _, err := time.Parse(time.RFC3339, d.Restore.Timestamp); err != nil {
				errors = append(errors, field.Invalid(restorePath.Child("timestamp"), d.Restore.Timestamp, "must be an RFC3339 timestamp"))
			}
		}
		if d.Restore.TxID != "" && !txIDPattern.MatchString(d.Restore.TxID) {
			errors = append(errors, field.Invalid(restorePath.Child("txid"), d.Restore.TxID, "must contain only hexadecimal characters"))
		}
	}
	if d.Replicate != nil {
		replicatePath := path.Child("replicate")
		if d.Replicate.SyncInterval.Duration < 0 {
			errors = append(errors, field.Invalid(replicatePath.Child("syncInterval"), d.Replicate.SyncInterval, "must be non-negative"))
		}
		errors = append(errors, validateLocalObjectReference(d.Replicate.ReplicaRef, replicatePath.Child("replicaRef"))...)
	}
	return errors
}

func validateLocalObjectReference(reference corev1.LocalObjectReference, path *field.Path) field.ErrorList {
	if reference.Name == "" {
		return field.ErrorList{field.Required(path.Child("name"), "name is required")}
	}
	if reasons := validation.IsDNS1123Subdomain(reference.Name); len(reasons) > 0 {
		return field.ErrorList{field.Invalid(path.Child("name"), reference.Name, strings.Join(reasons, "; "))}
	}
	return nil
}

// Validate checks that exactly one storage backend is configured and that it
// corresponds to the replica type.
func (r *ReplicaSpec) Validate(path *field.Path) field.ErrorList {
	if r.configuredBackends() != 1 || !r.hasBackendForType() {
		return field.ErrorList{field.Invalid(path, r.Type, "exactly one backend must be configured and match type")}
	}

	var errors field.ErrorList
	switch r.Type {
	case ReplicaTypeS3:
		errors = append(errors, validateRequiredString(r.S3.Bucket, path.Child("s3", "bucket"))...)
		errors = append(errors, validateRequiredString(r.S3.Path, path.Child("s3", "path"))...)
		errors = append(errors, validateS3Secrets(r.S3, path.Child("s3"))...)
	case ReplicaTypeGCS:
		errors = append(errors, validateRequiredString(r.GCS.Bucket, path.Child("gcs", "bucket"))...)
		errors = append(errors, validateRequiredString(r.GCS.Path, path.Child("gcs", "path"))...)
		errors = append(errors, validateGCSSecrets(r.GCS, path.Child("gcs"))...)
	case ReplicaTypeAzure:
		errors = append(errors, validateRequiredString(r.Azure.AccountName, path.Child("azure", "accountName"))...)
		errors = append(errors, validateRequiredString(r.Azure.Container, path.Child("azure", "container"))...)
		errors = append(errors, validateRequiredString(r.Azure.Path, path.Child("azure", "path"))...)
		errors = append(errors, validateAzureSecrets(r.Azure, path.Child("azure"))...)
	case ReplicaTypeFile:
		errors = append(errors, validateRequiredString(r.File.Path, path.Child("file", "path"))...)
	case ReplicaTypeNATS:
		errors = append(errors, validateRequiredString(r.NATS.URL, path.Child("nats", "url"))...)
		errors = append(errors, validateNATSSecrets(r.NATS, path.Child("nats"))...)
	case ReplicaTypeOSS:
		errors = append(errors, validateRequiredString(r.OSS.Bucket, path.Child("oss", "bucket"))...)
		errors = append(errors, validateRequiredString(r.OSS.Path, path.Child("oss", "path"))...)
		errors = append(errors, validateOSSSecrets(r.OSS, path.Child("oss"))...)
	case ReplicaTypeSFTP:
		errors = append(errors, validateRequiredString(r.SFTP.Host, path.Child("sftp", "host"))...)
		errors = append(errors, validateRequiredString(r.SFTP.User, path.Child("sftp", "user"))...)
		errors = append(errors, validateRequiredString(r.SFTP.Path, path.Child("sftp", "path"))...)
		errors = append(errors, validateSFTPSecrets(r.SFTP, path.Child("sftp"))...)
	case ReplicaTypeWebDAV:
		errors = append(errors, validateRequiredString(r.WebDAV.URL, path.Child("webdav", "url"))...)
		errors = append(errors, validateWebDAVSecrets(r.WebDAV, path.Child("webdav"))...)
	}
	return errors
}

func validateRequiredString(value string, path *field.Path) field.ErrorList {
	if strings.TrimSpace(value) == "" {
		return field.ErrorList{field.Required(path, "value is required")}
	}
	return nil
}

func (r *ReplicaSpec) configuredBackends() int {
	n := 0
	for _, configured := range []bool{
		r.S3 != nil, r.GCS != nil, r.Azure != nil, r.File != nil,
		r.NATS != nil, r.OSS != nil, r.SFTP != nil, r.WebDAV != nil,
	} {
		if configured {
			n++
		}
	}
	return n
}

func (r *ReplicaSpec) hasBackendForType() bool {
	switch r.Type {
	case ReplicaTypeS3:
		return r.S3 != nil
	case ReplicaTypeGCS:
		return r.GCS != nil
	case ReplicaTypeAzure:
		return r.Azure != nil
	case ReplicaTypeFile:
		return r.File != nil
	case ReplicaTypeNATS:
		return r.NATS != nil
	case ReplicaTypeOSS:
		return r.OSS != nil
	case ReplicaTypeSFTP:
		return r.SFTP != nil
	case ReplicaTypeWebDAV:
		return r.WebDAV != nil
	default:
		return false
	}
}

func validatePermissionMode(mode string, path *field.Path) field.ErrorList {
	if mode != "" && !permissionModePattern.MatchString(mode) {
		return field.ErrorList{field.Invalid(path, mode, "must be an octal permission string")}
	}
	return nil
}

func validateS3Secrets(spec *S3ReplicaSpec, path *field.Path) field.ErrorList {
	errors := validateSecretReference(spec.Credentials.AccessKeyID, path.Child("credentials", "accessKeyID"))
	return append(errors,
		validateSecretReference(spec.Credentials.SecretAccessKey, path.Child("credentials", "secretAccessKey"))...,
	)
}

func validateGCSSecrets(spec *GCSReplicaSpec, path *field.Path) field.ErrorList {
	return validateFileSecretReference(spec.ServiceAccountJSON, path.Child("serviceAccountJSON"))
}

func validateAzureSecrets(spec *AzureReplicaSpec, path *field.Path) field.ErrorList {
	return validateSecretReference(spec.AccountKey, path.Child("accountKey"))
}

func validateNATSSecrets(spec *NATSReplicaSpec, path *field.Path) field.ErrorList {
	var errors field.ErrorList
	for _, secret := range []struct {
		value *SecretReference
		name  string
	}{
		{spec.Username, "username"},
		{spec.Password, "password"},
		{spec.JWT, "jwt"},
		{spec.Seed, "seed"},
		{spec.NKey, "nkey"},
		{spec.Token, "token"},
	} {
		errors = append(errors, validateSecretReference(secret.value, path.Child(secret.name))...)
	}
	errors = append(errors, validateFileSecretReference(spec.Creds, path.Child("creds"))...)
	errors = append(errors, validateFileSecretReference(spec.ClientCert, path.Child("clientCert"))...)
	errors = append(errors, validateFileSecretReference(spec.ClientKey, path.Child("clientKey"))...)
	for i := range spec.RootCAs {
		errors = append(errors, validateFileSecretValue(spec.RootCAs[i], path.Child("rootCAs").Index(i))...)
	}
	if (spec.Username == nil) != (spec.Password == nil) {
		missing := "password"
		if spec.Username == nil {
			missing = "username"
		}
		errors = append(errors, field.Required(path.Child(missing), "username and password must be configured together"))
	}
	if (spec.JWT == nil) != (spec.Seed == nil) {
		missing := "seed"
		if spec.JWT == nil {
			missing = "jwt"
		}
		errors = append(errors, field.Required(path.Child(missing), "jwt and seed must be configured together"))
	}
	if (spec.ClientCert == nil) != (spec.ClientKey == nil) {
		missing := "clientKey"
		if spec.ClientCert == nil {
			missing = "clientCert"
		}
		errors = append(errors, field.Required(path.Child(missing), "client certificate and key must be configured together"))
	}

	mechanisms := []struct {
		name       string
		configured bool
	}{
		{"username", spec.Username != nil},
		{"jwt", spec.JWT != nil},
		{"creds", spec.Creds != nil},
		{"nkey", spec.NKey != nil},
		{"token", spec.Token != nil},
	}
	seen := false
	for _, mechanism := range mechanisms {
		if !mechanism.configured {
			continue
		}
		if seen {
			errors = append(errors, field.Invalid(path.Child(mechanism.name), true, "authentication mechanisms are mutually exclusive"))
		}
		seen = true
	}
	if spec.MaxReconnects != nil && *spec.MaxReconnects < -1 {
		errors = append(errors, field.Invalid(path.Child("maxReconnects"), *spec.MaxReconnects, "must be -1 or greater"))
	}
	if spec.ReconnectWait != nil && spec.ReconnectWait.Duration <= 0 {
		errors = append(errors, field.Invalid(path.Child("reconnectWait"), spec.ReconnectWait, "must be greater than zero"))
	}
	if spec.Timeout != nil && spec.Timeout.Duration <= 0 {
		errors = append(errors, field.Invalid(path.Child("timeout"), spec.Timeout, "must be greater than zero"))
	}
	return errors
}

func validateOSSSecrets(spec *OSSReplicaSpec, path *field.Path) field.ErrorList {
	errors := validateSecretReference(spec.AccessKeyID, path.Child("accessKeyID"))
	return append(errors,
		validateSecretReference(spec.AccessKeySecret, path.Child("accessKeySecret"))...,
	)
}

func validateSFTPSecrets(spec *SFTPReplicaSpec, path *field.Path) field.ErrorList {
	errors := validateSecretReference(spec.Password, path.Child("password"))
	return append(errors,
		validateFileSecretReference(spec.PrivateKey, path.Child("privateKey"))...,
	)
}

func validateWebDAVSecrets(spec *WebDAVReplicaSpec, path *field.Path) field.ErrorList {
	errors := validateSecretReference(spec.Username, path.Child("username"))
	return append(errors,
		validateSecretReference(spec.Password, path.Child("password"))...,
	)
}

func validateSecretReference(secret *SecretReference, path *field.Path) field.ErrorList {
	if secret == nil {
		return nil
	}
	errors := validateSecretValue(*secret, path)
	if secret.SecretKeyRef.Optional != nil && *secret.SecretKeyRef.Optional {
		errors = append(errors, field.Forbidden(
			path.Child("secretKeyRef", "optional"),
			"optional environment-backed Secret references are not supported; omit the reference instead",
		))
	}
	return errors
}

func validateFileSecretReference(secret *SecretReference, path *field.Path) field.ErrorList {
	if secret == nil {
		return nil
	}
	return validateFileSecretValue(*secret, path)
}

func validateSecretValue(secret SecretReference, path *field.Path) field.ErrorList {
	var errors field.ErrorList
	if secret.SecretKeyRef.Name == "" {
		errors = append(errors, field.Required(path.Child("secretKeyRef", "name"), "secret name is required"))
	} else if reasons := validation.IsDNS1123Subdomain(secret.SecretKeyRef.Name); len(reasons) > 0 {
		errors = append(errors, field.Invalid(path.Child("secretKeyRef", "name"), secret.SecretKeyRef.Name, strings.Join(reasons, "; ")))
	}
	if secret.SecretKeyRef.Key == "" {
		errors = append(errors, field.Required(path.Child("secretKeyRef", "key"), "secret key is required"))
	} else if reasons := validation.IsConfigMapKey(secret.SecretKeyRef.Key); len(reasons) > 0 {
		errors = append(errors, field.Invalid(path.Child("secretKeyRef", "key"), secret.SecretKeyRef.Key, strings.Join(reasons, "; ")))
	}
	return errors
}

func validateFileSecretValue(secret SecretReference, path *field.Path) field.ErrorList {
	errors := validateSecretValue(secret, path)
	if secret.SecretKeyRef.Optional != nil && *secret.SecretKeyRef.Optional {
		errors = append(errors, field.Forbidden(
			path.Child("secretKeyRef", "optional"),
			"optional file-backed Secret references are not supported; omit the reference instead",
		))
	}
	return errors
}
