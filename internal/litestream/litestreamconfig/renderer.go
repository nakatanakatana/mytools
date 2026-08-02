package litestreamconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// ConfigMountDir is where the generated ConfigMap must be mounted: the
	// rendered scripts reference their configuration through this path.
	ConfigMountDir = "/etc/litestream"

	// ReplicateConfigFile holds every replicating database and is only
	// rendered when at least one database replicates.
	ReplicateConfigFile = "replicate.yml"
)

// RenderedConfig is the pure, non-secret rendering of a Litestream resource:
// ConfigMap data, the Secret bindings the webhook must project into
// containers, and a stable revision hash of the ConfigMap data and
// non-secret credential binding metadata.
type RenderedConfig struct {
	Data        map[string]string
	Credentials []CredentialBinding
	Hash        string
}

// configFile is the top-level shape of a Litestream YAML configuration file.
type configFile struct {
	DBs []dbEntry `yaml:"dbs"`
}

type dbEntry struct {
	Path    string      `yaml:"path"`
	Replica replicaYAML `yaml:"replica"`
}

// replicaYAML is a private DTO covering every backend's Litestream field
// names. Unused fields are omitted from output via `omitempty`.
type replicaYAML struct {
	Type   string `yaml:"type"`
	Bucket string `yaml:"bucket,omitempty"`
	Path   string `yaml:"path,omitempty"`
	Region string `yaml:"region,omitempty"`

	Endpoint       string `yaml:"endpoint,omitempty"`
	ForcePathStyle *bool  `yaml:"force-path-style,omitempty"`
	SkipVerify     *bool  `yaml:"skip-verify,omitempty"`

	AccessKeyID     string `yaml:"access-key-id,omitempty"`
	SecretAccessKey string `yaml:"secret-access-key,omitempty"`

	AccountName string `yaml:"account-name,omitempty"`
	AccountKey  string `yaml:"account-key,omitempty"`

	Host             string `yaml:"host,omitempty"`
	User             string `yaml:"user,omitempty"`
	Password         string `yaml:"password,omitempty"`
	KeyPath          string `yaml:"key-path,omitempty"`
	ConcurrentWrites *bool  `yaml:"concurrent-writes,omitempty"`

	WebDAVURL      string `yaml:"webdav-url,omitempty"`
	WebDAVUsername string `yaml:"webdav-username,omitempty"`
	WebDAVPassword string `yaml:"webdav-password,omitempty"`

	URL           string   `yaml:"url,omitempty"`
	Username      string   `yaml:"username,omitempty"`
	JWT           string   `yaml:"jwt,omitempty"`
	Seed          string   `yaml:"seed,omitempty"`
	Creds         string   `yaml:"creds,omitempty"`
	NKey          string   `yaml:"nkey,omitempty"`
	Token         string   `yaml:"token,omitempty"`
	RootCAs       []string `yaml:"root-cas,omitempty"`
	ClientCert    string   `yaml:"client-cert,omitempty"`
	ClientKey     string   `yaml:"client-key,omitempty"`
	MaxReconnects *int     `yaml:"max-reconnects,omitempty"`
	ReconnectWait string   `yaml:"reconnect-wait,omitempty"`
	Timeout       string   `yaml:"timeout,omitempty"`

	SyncInterval string `yaml:"sync-interval,omitempty"`
	AutoRecover  bool   `yaml:"auto-recover,omitempty"`
}

// Render turns a resolved Litestream input into ConfigMap data and
// credential bindings. Secret values are never read; only "${ENV}"
// placeholders and SecretKeySelector bindings are produced.
func Render(input Input) (RenderedConfig, error) {
	if err := ValidateInput(input); err != nil {
		return RenderedConfig{}, err
	}

	data := make(map[string]string)
	var credentials []CredentialBinding
	var replicateEntries []dbEntry

	for _, db := range input.Databases {
		perms := input.Injection.Permissions

		switch {
		case db.Restore != nil && db.Replicate == nil:
			creds, err := renderRestoreOnly(db, perms, data)
			if err != nil {
				return RenderedConfig{}, err
			}
			credentials = append(credentials, creds...)

		case !db.Clone && db.Replicate != nil:
			creds, entry, err := renderReplicate(db, perms, data)
			if err != nil {
				return RenderedConfig{}, err
			}
			credentials = append(credentials, creds...)
			replicateEntries = append(replicateEntries, entry)

		case db.Clone:
			creds, entry, err := renderClone(db, perms, data)
			if err != nil {
				return RenderedConfig{}, err
			}
			credentials = append(credentials, creds...)
			replicateEntries = append(replicateEntries, entry)

		default:
			return RenderedConfig{}, fmt.Errorf("litestreamconfig: database must have a restore source or replication destination")
		}
	}

	if len(replicateEntries) > 0 {
		doc, err := marshalConfig(replicateEntries)
		if err != nil {
			return RenderedConfig{}, err
		}
		data[ReplicateConfigFile] = doc
	}

	sortCredentialBindings(credentials)

	return RenderedConfig{
		Data:        data,
		Credentials: credentials,
		Hash:        hashRenderedConfig(data, credentials),
	}, nil
}

func renderRestoreOnly(db Database, perms v1alpha1.PermissionsSpec, data map[string]string) ([]CredentialBinding, error) {
	purpose := RestoreContainerPurpose(db.Name)
	builder := newCredentialBuilder(db.Name, credentialRoleSource, backendName(db.Restore.Replica.Type), purpose)
	replica := renderReplica(builder, db.Restore.Replica)

	doc, err := marshalConfig([]dbEntry{{Path: db.Path, Replica: replica}})
	if err != nil {
		return nil, err
	}
	data[restoreConfigFileName(db.Name)] = doc

	script, err := buildSingleRestoreScript(db, perms, restoreConfigFileName(db.Name), restorePolicyFromSpec(db.Restore))
	if err != nil {
		return nil, err
	}
	data[RestoreScriptFileName(db.Name)] = script

	return builder.bindings, nil
}

func renderReplicate(db Database, perms v1alpha1.PermissionsSpec, data map[string]string) ([]CredentialBinding, dbEntry, error) {
	destBuilder := newCredentialBuilder(db.Name, credentialRoleDestination, backendName(db.Replicate.Replica.Type), ReplicateContainerPurpose)
	destReplica := renderReplica(destBuilder, db.Replicate.Replica)
	destReplica.SyncInterval = formatDuration(db.Replicate.SyncInterval)
	destReplica.AutoRecover = db.Replicate.AutoRecover
	entry := dbEntry{Path: db.Path, Replica: destReplica}

	var credentials []CredentialBinding
	credentials = append(credentials, destBuilder.bindings...)

	purpose := RestoreContainerPurpose(db.Name)
	var script string
	var err error

	if db.Restore != nil {
		srcBuilder := newCredentialBuilder(db.Name, credentialRoleSource, backendName(db.Restore.Replica.Type), purpose)
		srcReplica := renderReplica(srcBuilder, db.Restore.Replica)

		doc, marshalErr := marshalConfig([]dbEntry{{Path: db.Path, Replica: srcReplica}})
		if marshalErr != nil {
			return nil, dbEntry{}, marshalErr
		}
		data[restoreConfigFileName(db.Name)] = doc
		credentials = append(credentials, srcBuilder.bindings...)

		script, err = buildSingleRestoreScript(db, perms, restoreConfigFileName(db.Name), restorePolicyFromSpec(db.Restore))
	} else {
		// The replicate destination doubles as the restore source: safely
		// recover the database only when it does not already exist.
		doc, marshalErr := marshalConfig([]dbEntry{{Path: db.Path, Replica: destReplica}})
		if marshalErr != nil {
			return nil, dbEntry{}, marshalErr
		}
		data[restoreConfigFileName(db.Name)] = doc
		credentials = append(credentials, destBuilder.rebind(purpose)...)

		script, err = buildSingleRestoreScript(db, perms, restoreConfigFileName(db.Name), defaultRestorePolicy())
	}
	if err != nil {
		return nil, dbEntry{}, err
	}
	data[RestoreScriptFileName(db.Name)] = script

	return credentials, entry, nil
}

func renderClone(db Database, perms v1alpha1.PermissionsSpec, data map[string]string) ([]CredentialBinding, dbEntry, error) {
	purpose := RestoreContainerPurpose(db.Name)

	destBuilder := newCredentialBuilder(db.Name, credentialRoleDestination, backendName(db.Replicate.Replica.Type), ReplicateContainerPurpose)
	destReplica := renderReplica(destBuilder, db.Replicate.Replica)
	destReplica.SyncInterval = formatDuration(db.Replicate.SyncInterval)
	destReplica.AutoRecover = db.Replicate.AutoRecover
	entry := dbEntry{Path: db.Path, Replica: destReplica}

	var credentials []CredentialBinding
	credentials = append(credentials, destBuilder.bindings...)

	destDoc, err := marshalConfig([]dbEntry{{Path: db.Path, Replica: destReplica}})
	if err != nil {
		return nil, dbEntry{}, err
	}
	destFileName := cloneConfigFileName(db.Name, "destination")
	data[destFileName] = destDoc
	credentials = append(credentials, destBuilder.rebind(purpose)...)

	srcBuilder := newCredentialBuilder(db.Name, credentialRoleSource, backendName(db.Restore.Replica.Type), purpose)
	srcReplica := renderReplica(srcBuilder, db.Restore.Replica)
	srcDoc, err := marshalConfig([]dbEntry{{Path: db.Path, Replica: srcReplica}})
	if err != nil {
		return nil, dbEntry{}, err
	}
	srcFileName := cloneConfigFileName(db.Name, "source")
	data[srcFileName] = srcDoc
	credentials = append(credentials, srcBuilder.bindings...)

	script, err := buildCloneScript(db, perms, destFileName, srcFileName)
	if err != nil {
		return nil, dbEntry{}, err
	}
	data[RestoreScriptFileName(db.Name)] = script

	return credentials, entry, nil
}

func restoreConfigFileName(databaseName string) string {
	return "restore-" + databaseName + ".yml"
}

func cloneConfigFileName(databaseName, role string) string {
	return "restore-" + databaseName + "-" + role + ".yml"
}

// RestoreScriptFileName is the ConfigMap key of the restore script the
// webhook must run in databaseName's init container.
func RestoreScriptFileName(databaseName string) string {
	return "restore-" + databaseName + ".sh"
}

func backendName(t v1alpha1.ReplicaType) string {
	return strings.ToUpper(string(t))
}

func marshalConfig(entries []dbEntry) (string, error) {
	doc, err := yaml.Marshal(configFile{DBs: entries})
	if err != nil {
		return "", fmt.Errorf("litestreamconfig: marshal config: %w", err)
	}
	return string(doc), nil
}

// hashRenderedConfig returns a deterministic revision identity for every
// input the webhook uses to mutate a Pod. Credential bindings contribute only
// their projection metadata; Secret values are never read or included.
func hashRenderedConfig(data map[string]string, credentials []CredentialBinding) string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte("data"))
		h.Write([]byte{0})
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(data[k]))
		h.Write([]byte{0})
	}
	for _, credential := range credentials {
		h.Write([]byte("credential"))
		h.Write([]byte{0})
		h.Write([]byte(credential.ContainerPurpose))
		h.Write([]byte{0})
		h.Write([]byte(credential.EnvName))
		h.Write([]byte{0})
		h.Write([]byte(credential.SecretKeyRef.Name))
		h.Write([]byte{0})
		h.Write([]byte(credential.SecretKeyRef.Key))
		h.Write([]byte{0})
		if credential.SecretKeyRef.Optional == nil {
			h.Write([]byte("optional:nil"))
		} else if *credential.SecretKeyRef.Optional {
			h.Write([]byte("optional:true"))
		} else {
			h.Write([]byte("optional:false"))
		}
		h.Write([]byte{0})
		h.Write([]byte(credential.FileMountPath))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func formatDuration(d metav1.Duration) string {
	if d.Duration == 0 {
		return ""
	}
	return d.Duration.String()
}

func chmodLines(db Database, perms v1alpha1.PermissionsSpec) (string, error) {
	if perms.DatabaseMode == "" && perms.DirectoryMode == "" {
		return "", nil
	}

	dbPath, err := shellQuote(db.Path)
	if err != nil {
		return "", err
	}
	lines := fmt.Sprintf("if [ -e %s ]; then\n", dbPath)
	// The restore container normally runs as a non-root UID. Permission
	// changes are part of the configured restore contract, so an inability to
	// apply either mode must fail the init container rather than silently
	// starting with different permissions.
	if perms.DatabaseMode != "" {
		lines += fmt.Sprintf("  if ! chmod %s %s; then\n", perms.DatabaseMode, dbPath)
		lines += "    echo 'error: unable to apply Litestream permissions' >&2\n"
		lines += "    exit 1\n"
	}
	if perms.DatabaseMode != "" {
		lines += "  fi\n"
	}
	if path.Dir(db.Path) == "/" {
		return lines + "fi\n", nil
	}
	if perms.DirectoryMode == "" {
		return lines + "fi\n", nil
	}
	dir, err := shellQuote(path.Dir(db.Path))
	if err != nil {
		return "", err
	}
	lines += fmt.Sprintf("  if ! chmod 2%s %s; then\n", perms.DirectoryMode[1:], dir)
	lines += "    echo 'error: unable to apply Litestream permissions' >&2\n"
	lines += "    exit 1\n"
	lines += "  fi\n"
	return lines + "fi\n", nil
}

func configPath(fileName string) string {
	return ConfigMountDir + "/" + fileName
}

func restoreLine(flags []string, configFile, dbPath string) (string, error) {
	quotedFlags, err := quoteFlags(flags)
	if err != nil {
		return "", err
	}
	quotedConfigFile, err := shellQuote(configFile)
	if err != nil {
		return "", err
	}
	quotedDBPath, err := shellQuote(dbPath)
	if err != nil {
		return "", err
	}
	return "litestream restore " + strings.Join(quotedFlags, " ") + " \\\n  -config " + quotedConfigFile + " " + quotedDBPath + "\n", nil
}

func quoteFlags(flags []string) ([]string, error) {
	quotedFlags := make([]string, 0, len(flags))
	for _, flag := range flags {
		quoted, err := shellQuote(flag)
		if err != nil {
			return nil, err
		}
		quotedFlags = append(quotedFlags, quoted)
	}
	return quotedFlags, nil
}

// stagedRestoreLine restores overwrite operations to a temporary file in the
// database's directory. Litestream removes the output path before checking
// whether a backup exists, so using the real database path with -force could
// destroy a valid local database when the replica is empty.
func stagedRestoreLine(flags []string, configFile, dbPath string) (string, error) {
	quotedFlags, err := quoteFlags(flags)
	if err != nil {
		return "", err
	}
	quotedConfigFile, err := shellQuote(configFile)
	if err != nil {
		return "", err
	}
	quotedDBPath, err := shellQuote(dbPath)
	if err != nil {
		return "", err
	}
	temporaryTemplate, err := shellQuote(path.Join(path.Dir(dbPath), ".litestream-restore-XXXXXX"))
	if err != nil {
		return "", err
	}
	quotedWAL, err := shellQuote(dbPath + "-wal")
	if err != nil {
		return "", err
	}
	quotedSHM, err := shellQuote(dbPath + "-shm")
	if err != nil {
		return "", err
	}
	quotedJournal, err := shellQuote(dbPath + "-journal")
	if err != nil {
		return "", err
	}

	return "restore_tmp=$(mktemp " + temporaryTemplate + ")\n" +
		"trap 'rm -f \"$restore_tmp\"' EXIT HUP INT TERM\n" +
		"litestream restore " + strings.Join(quotedFlags, " ") + " \\\n  -config " + quotedConfigFile + " -o \"$restore_tmp\" " + quotedDBPath + "\n" +
		"if [ -s \"$restore_tmp\" ]; then\n" +
		"  mv \"$restore_tmp\" " + quotedDBPath + "\n" +
		"  rm -f " + quotedWAL + " " + quotedSHM + " " + quotedJournal + "\n" +
		"fi\n", nil
}

func buildSingleRestoreScript(db Database, perms v1alpha1.PermissionsSpec, configFileName string, policy restorePolicy) (string, error) {
	chmod, err := chmodLines(db, perms)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("#!/bin/sh\nset -eu\n")
	restore, err := buildRestoreLine(policy, configPath(configFileName), db.Path)
	if err != nil {
		return "", err
	}
	b.WriteString(restore)
	b.WriteString(chmod)
	return b.String(), nil
}

func buildCloneScript(db Database, perms v1alpha1.PermissionsSpec, destFileName, srcFileName string) (string, error) {
	destCfg, err := shellQuote(configPath(destFileName))
	if err != nil {
		return "", err
	}
	dbPath, err := shellQuote(db.Path)
	if err != nil {
		return "", err
	}
	chmod, err := chmodLines(db, perms)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("#!/bin/sh\nset -eu\n")

	if db.ClonePolicy == v1alpha1.ClonePolicyRequireEmpty {
		preflight, err := buildRequireEmptyPreflight(db.Name, destCfg, dbPath)
		if err != nil {
			return "", err
		}
		b.WriteString(preflight)
	}

	destinationRestore, err := buildRestoreLine(defaultRestorePolicy(), configPath(destFileName), db.Path)
	if err != nil {
		return "", err
	}
	sourceRestore, err := buildRestoreLine(restorePolicyFromSpec(db.Restore), configPath(srcFileName), db.Path)
	if err != nil {
		return "", err
	}
	b.WriteString(destinationRestore)
	b.WriteString(sourceRestore)
	b.WriteString(chmod)
	return b.String(), nil
}

func buildRestoreLine(policy restorePolicy, configFile, dbPath string) (string, error) {
	if policy.ifDatabaseExists == v1alpha1.IfDatabaseExistsOverwrite {
		return stagedRestoreLine(policy.flags(), configFile, dbPath)
	}
	return restoreLine(policy.flags(), configFile, dbPath)
}

func buildRequireEmptyPreflight(databaseName, destCfg, dbPath string) (string, error) {
	// databaseName comes from the CR and is otherwise unconstrained, so it
	// must go through the same quoting as any other untrusted value before
	// it reaches the shell (here, as a printf argument rather than being
	// interpolated into the message text directly).
	nameArg, err := shellQuote(databaseName)
	if err != nil {
		return "", err
	}
	// The restore dry-run is format-aware: unlike the ltx command it detects
	// both legacy v0.3 replicas and current LTX replicas. Any command failure or
	// unrecognized output is unknown state and must abort the clone rather than
	// weakening require-empty's guarantee.
	return fmt.Sprintf(
		"restore_plan=$(litestream restore -dry-run -json -if-replica-exists -config %s %s 2>&1) || {\n"+
			"  case \"$restore_plan\" in\n"+
			"  *'no matching backup files available'*|*'no matching backups found'*) restore_plan='{\"files\":[]}' ;;\n"+
			"  *)\n"+
			"  printf 'litestream: unable to inspect destination replica for database %%s; require-empty policy refuses to continue\\n' %s >&2\n"+
			"  exit 1\n"+
			"  ;;\n"+
			"  esac\n"+
			"}\n"+
			"compact_plan=$(printf '%%s' \"$restore_plan\" | tr -d '[:space:]')\n"+
			"case \"$compact_plan\" in\n"+
			"  *'\"files\":[]'*) ;;\n"+
			"  *'\"files\":['*)\n"+
			"  printf 'litestream: destination replica for database %%s already contains data; require-empty policy refuses to continue\\n' %s >&2\n"+
			"  exit 1\n"+
			"  ;;\n"+
			"  *)\n"+
			"  printf 'litestream: unable to inspect destination replica for database %%s; require-empty policy refuses to continue\\n' %s >&2\n"+
			"  exit 1\n"+
			"  ;;\n"+
			"esac\n",
		destCfg, dbPath, nameArg, nameArg, nameArg,
	), nil
}

// shellQuote single-quotes s for safe use as one POSIX shell argument. It
// rejects values that cannot be represented as a single argument on one
// line: NUL bytes and newlines.
func shellQuote(s string) (string, error) {
	if strings.ContainsAny(s, "\x00\n") {
		return "", fmt.Errorf("litestreamconfig: refusing to render unsafe shell argument %q", s)
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'", nil
}
