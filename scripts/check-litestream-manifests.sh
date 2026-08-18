#!/usr/bin/env bash
# check-litestream-manifests.sh
# Verifies that the generated Litestream controller artifacts (deepcopy code,
# CRDs, RBAC, and the webhook configuration) are up to date with the
# kubebuilder markers in api/litestream/, internal/litestream/controller/, and
# internal/litestream/webhook/.
#
# It runs `make generate manifests` against the real working tree and
# compares the result with `git diff`, then restores the pre-run contents of
# every generated path so the check never leaves the working tree
# modified, regardless of whether it passes or fails.
#
# Usage:
#   bash scripts/check-litestream-manifests.sh

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# Paths that `make generate manifests` is allowed to write to. Keep this list
# limited to generated files and directories: their parent directories also
# contain hand-written source, tests, samples, and deployment configuration.
GENERATED_PATHS=(
  api/litestream/v1alpha1/zz_generated.deepcopy.go
  config/litestream-controller/crd/bases
  config/litestream-controller/rbac
  config/litestream-controller/webhook/manifests.yaml
)

TMP_DIR="$(mktemp -d -t litestream-manifests-check.XXXXXX)"
RENDERED_WEBHOOK_MANIFEST="${TMP_DIR}/webhook.yaml"

restore_generated_paths() {
  local path
  for path in "${GENERATED_PATHS[@]}"; do
    rm -rf "${REPO_ROOT:?}/${path:?}"
    if [[ -e "${TMP_DIR}/${path}" ]]; then
      mkdir -p "$(dirname "${REPO_ROOT}/${path}")"
      cp -a "${TMP_DIR}/${path}" "${REPO_ROOT}/${path}"
    fi
  done
  rm -rf "${TMP_DIR}"
}
trap restore_generated_paths EXIT

for path in "${GENERATED_PATHS[@]}"; do
  if [[ -e "${path}" ]]; then
    mkdir -p "$(dirname "${TMP_DIR}/${path}")"
    cp -a "${path}" "${TMP_DIR}/${path}"
  fi
done

if ! make generate manifests; then
  echo "error: 'make generate manifests' failed" >&2
  exit 1
fi

status=0

if ! git diff --exit-code -- "${GENERATED_PATHS[@]}"; then
  echo "error: generated manifests are out of date; run 'make generate manifests' and commit the result" >&2
  status=1
fi

untracked="$(git ls-files --others --exclude-standard -- "${GENERATED_PATHS[@]}")"
if [[ -n "${untracked}" ]]; then
  echo "error: 'make generate manifests' produced untracked files:" >&2
  printf '%s\n' "${untracked}" >&2
  status=1
fi

if ! kustomize build config/litestream-controller/default >"${RENDERED_WEBHOOK_MANIFEST}"; then
  echo "error: failed to render the Litestream controller manifest" >&2
  exit 1
fi

mutating_webhook="$(awk '
  /^kind: MutatingWebhookConfiguration$/ { in_webhook = 1 }
  in_webhook { print }
  in_webhook && /^---$/ { exit }
' "${RENDERED_WEBHOOK_MANIFEST}")"

validating_webhook="$(awk '
  /^kind: ValidatingWebhookConfiguration$/ { in_webhook = 1 }
  in_webhook { print }
  in_webhook && /^---$/ { exit }
' "${RENDERED_WEBHOOK_MANIFEST}")"

required_lines=(
  'failurePolicy: Fail'
  'litestream.mytools.nakatanakatana.app/injection: enabled'
  'kubernetes.io/metadata.name'
  'litestream-controller-system'
  'matchConditions:'
  'name: litestream-injection-request'
)

for required_line in "${required_lines[@]}"; do
  if ! grep -Fq "${required_line}" <<<"${mutating_webhook}"; then
    echo "error: rendered mutating webhook is missing required setting: ${required_line}" >&2
    status=1
  fi
done

if ! perl -0ne "exit !(m{matchConditions:\\s*-\\s+expression:\\s*has\\(object\\.metadata\\.annotations\\)\\s*&&\\s*'litestream\\.mytools\\.nakatanakatana\\.app/inject'\\s+in\\s+object\\.metadata\\.annotations\\s*&&\\s*object\\.metadata\\.annotations\\['litestream\\.mytools\\.nakatanakatana\\.app/inject'\\]\\.trim\\(\\)\\s*!=\\s*''\\s+name:\\s*litestream-injection-request}s)" <<<"${mutating_webhook}"; then
  echo "error: rendered mutating webhook is missing the non-empty trimmed inject annotation match condition" >&2
  status=1
fi

workload_validating_webhook="$(perl -0ne '@items = split(/(?=^- admissionReviewVersions:)/m, $_); for $item (@items) { print $item if $item =~ /^  name: vlitestreamworkload\.litestream-controller\.mytools\.nakatanakatana\.app$/m; }' <<<"${validating_webhook}")"

validating_required_lines=(
	'name: vlitestreamworkload.litestream-controller.mytools.nakatanakatana.app'
	'litestream.mytools.nakatanakatana.app/injection: enabled'
	'kubernetes.io/metadata.name'
	'litestream-controller-system'
	'deployments/scale'
	'statefulsets/scale'
)

for required_line in "${validating_required_lines[@]}"; do
	if ! grep -Fq "${required_line}" <<<"${workload_validating_webhook}"; then
		echo "error: rendered workload validating webhook is missing required setting: ${required_line}" >&2
		status=1
	fi
done

exit "${status}"
