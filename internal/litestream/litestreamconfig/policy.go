package litestreamconfig

import "github.com/nakatanakatana/mytools/api/litestream/v1alpha1"

// restorePolicy resolves a RestoreSpec's policy fields to the concrete
// `litestream restore` flags they control, applying the same defaults the
// CRD applies (skip, skip, quick) so that Render behaves identically whether
// or not the API server has already defaulted the resource.
type restorePolicy struct {
	ifDatabaseExists v1alpha1.IfDatabaseExistsPolicy
	ifReplicaMissing v1alpha1.IfReplicaMissingPolicy
	integrityCheck   v1alpha1.IntegrityCheck
	timestamp        string
	txID             string
}

func defaultRestorePolicy() restorePolicy {
	return restorePolicy{
		ifDatabaseExists: v1alpha1.IfDatabaseExistsSkip,
		ifReplicaMissing: v1alpha1.IfReplicaMissingSkip,
		integrityCheck:   v1alpha1.IntegrityCheckQuick,
	}
}

func restorePolicyFromSpec(spec *Restore) restorePolicy {
	policy := defaultRestorePolicy()
	if spec.IfDatabaseExists != "" {
		policy.ifDatabaseExists = spec.IfDatabaseExists
	}
	if spec.IfReplicaMissing != "" {
		policy.ifReplicaMissing = spec.IfReplicaMissing
	}
	if spec.IntegrityCheck != "" {
		policy.integrityCheck = spec.IntegrityCheck
	}
	policy.timestamp = spec.Timestamp
	policy.txID = spec.TxID
	return policy
}

// flags renders the `litestream restore` arguments controlled by this
// policy, in the fixed order: existence handling, replica-missing handling,
// integrity check, and an optional point-in-time selector.
func (p restorePolicy) flags() []string {
	var flags []string

	switch p.ifDatabaseExists {
	case v1alpha1.IfDatabaseExistsOverwrite:
		flags = append(flags, "-force")
	case v1alpha1.IfDatabaseExistsFail:
		// No flag: Litestream's default behavior is to fail when the
		// database already exists.
	default:
		flags = append(flags, "-if-db-not-exists")
	}

	if p.ifReplicaMissing != v1alpha1.IfReplicaMissingFail {
		flags = append(flags, "-if-replica-exists")
	}

	integrity := p.integrityCheck
	if integrity == "" {
		integrity = v1alpha1.IntegrityCheckQuick
	}
	flags = append(flags, "-integrity-check", string(integrity))

	switch {
	case p.timestamp != "":
		flags = append(flags, "-timestamp", p.timestamp)
	case p.txID != "":
		flags = append(flags, "-txid", p.txID)
	}

	return flags
}
