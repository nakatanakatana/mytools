package litestreamconfig

import "github.com/nakatanakatana/mytools/api/litestream/v1alpha1"

// renderReplica dispatches to the backend-specific builder that matches
// replica.Type, recording any Secret-backed fields on builder as it goes.
// Exactly one backend pointer is expected to be set; this is guaranteed by
// API validation and the resolver before Render receives the resolved input.
func renderReplica(builder *credentialBuilder, replica v1alpha1.ReplicaSpec) replicaYAML {
	switch replica.Type {
	case v1alpha1.ReplicaTypeS3:
		return renderS3(builder, replica.S3)
	case v1alpha1.ReplicaTypeGCS:
		return renderGCS(builder, replica.GCS)
	case v1alpha1.ReplicaTypeAzure:
		return renderAzure(builder, replica.Azure)
	case v1alpha1.ReplicaTypeFile:
		return renderFile(replica.File)
	case v1alpha1.ReplicaTypeNATS:
		return renderNATS(builder, replica.NATS)
	case v1alpha1.ReplicaTypeOSS:
		return renderOSS(builder, replica.OSS)
	case v1alpha1.ReplicaTypeSFTP:
		return renderSFTP(builder, replica.SFTP)
	case v1alpha1.ReplicaTypeWebDAV:
		return renderWebDAV(builder, replica.WebDAV)
	default:
		return replicaYAML{Type: string(replica.Type)}
	}
}

func renderS3(b *credentialBuilder, spec *v1alpha1.S3ReplicaSpec) replicaYAML {
	return replicaYAML{
		Type:            "s3",
		Bucket:          spec.Bucket,
		Path:            spec.Path,
		Region:          spec.Region,
		Endpoint:        spec.Endpoint,
		ForcePathStyle:  spec.ForcePathStyle,
		SkipVerify:      spec.SkipVerify,
		AccessKeyID:     b.env("ACCESS_KEY_ID", spec.Credentials.AccessKeyID),
		SecretAccessKey: b.env("SECRET_ACCESS_KEY", spec.Credentials.SecretAccessKey),
	}
}

func renderGCS(b *credentialBuilder, spec *v1alpha1.GCSReplicaSpec) replicaYAML {
	// GCS credentials are not a config-file field: Litestream discovers a
	// service account key file through GOOGLE_APPLICATION_CREDENTIALS.
	b.fileWithEnv("GOOGLE_APPLICATION_CREDENTIALS", "service-account.json", spec.ServiceAccountJSON)
	return replicaYAML{
		Type:   "gs",
		Bucket: spec.Bucket,
		Path:   spec.Path,
	}
}

func renderAzure(b *credentialBuilder, spec *v1alpha1.AzureReplicaSpec) replicaYAML {
	return replicaYAML{
		Type:        "abs",
		AccountName: spec.AccountName,
		Bucket:      spec.Container,
		Path:        spec.Path,
		Endpoint:    spec.Endpoint,
		AccountKey:  b.env("ACCOUNT_KEY", spec.AccountKey),
	}
}

func renderFile(spec *v1alpha1.FileReplicaSpec) replicaYAML {
	return replicaYAML{
		Type: "file",
		Path: spec.Path,
	}
}

func renderNATS(b *credentialBuilder, spec *v1alpha1.NATSReplicaSpec) replicaYAML {
	y := replicaYAML{
		Type:       "nats",
		URL:        spec.URL,
		Username:   b.env("USERNAME", spec.Username),
		Password:   b.env("PASSWORD", spec.Password),
		JWT:        b.env("JWT", spec.JWT),
		Seed:       b.env("SEED", spec.Seed),
		NKey:       b.env("NKEY", spec.NKey),
		Token:      b.env("TOKEN", spec.Token),
		Creds:      b.file("creds", spec.Creds),
		ClientCert: b.file("client-cert", spec.ClientCert),
		ClientKey:  b.file("client-key", spec.ClientKey),
		RootCAs:    b.fileList("root-ca", spec.RootCAs),
	}
	if spec.MaxReconnects != nil {
		v := *spec.MaxReconnects
		y.MaxReconnects = &v
	}
	if spec.ReconnectWait != nil {
		y.ReconnectWait = spec.ReconnectWait.Duration.String()
	}
	if spec.Timeout != nil {
		y.Timeout = spec.Timeout.Duration.String()
	}
	return y
}

func renderOSS(b *credentialBuilder, spec *v1alpha1.OSSReplicaSpec) replicaYAML {
	return replicaYAML{
		Type:        "oss",
		Bucket:      spec.Bucket,
		Path:        spec.Path,
		Endpoint:    spec.Endpoint,
		Region:      spec.Region,
		AccessKeyID: b.env("ACCESS_KEY_ID", spec.AccessKeyID),
		// Litestream's OSS replica reuses the same "secret-access-key" field
		// as S3, not "access-key-secret".
		SecretAccessKey: b.env("ACCESS_KEY_SECRET", spec.AccessKeySecret),
	}
}

func renderSFTP(b *credentialBuilder, spec *v1alpha1.SFTPReplicaSpec) replicaYAML {
	return replicaYAML{
		Type:             "sftp",
		Host:             spec.Host,
		User:             spec.User,
		Path:             spec.Path,
		Password:         b.env("PASSWORD", spec.Password),
		KeyPath:          b.file("private-key", spec.PrivateKey),
		ConcurrentWrites: spec.ConcurrentWrites,
	}
}

func renderWebDAV(b *credentialBuilder, spec *v1alpha1.WebDAVReplicaSpec) replicaYAML {
	return replicaYAML{
		Type:           "webdav",
		WebDAVURL:      spec.URL,
		Path:           spec.Path,
		WebDAVUsername: b.env("USERNAME", spec.Username),
		WebDAVPassword: b.env("PASSWORD", spec.Password),
		SkipVerify:     spec.SkipVerify,
	}
}
