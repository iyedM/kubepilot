#!/usr/bin/env bash
# Download the pinned kind and kubectl into .tools/bin and verify their
# sha256 against lab/versions.env. A mismatch is fatal.
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/lib.sh"

mkdir -p "${BIN_DIR}"

# install_tool NAME VERSION SHA256 URL
install_tool() {
  local name="$1" version="$2" sha="$3" url="$4"
  local dest="${BIN_DIR}/${name}" stamp="${BIN_DIR}/.${name}.version"

  if [[ -x "${dest}" && -f "${stamp}" && "$(cat "${stamp}")" == "${version}" ]]; then
    log "${name} ${version} already installed"
    return
  fi

  log "downloading ${name} ${version} from ${url}"
  local tmp
  tmp="$(mktemp)"
  if ! curl -fsSL --retry 3 --retry-all-errors --connect-timeout 20 -o "${tmp}" "${url}"; then
    rm -f "${tmp}"
    die "download failed: ${url}"
  fi
  if ! echo "${sha}  ${tmp}" | sha256sum --check --status; then
    local got
    got="$(sha256sum "${tmp}" | cut -d' ' -f1)"
    rm -f "${tmp}"
    die "${name} ${version}: sha256 mismatch (expected ${sha}, got ${got})"
  fi
  install -m 0755 "${tmp}" "${dest}"
  rm -f "${tmp}"
  echo "${version}" > "${stamp}"
  log "${name} ${version} installed, sha256 verified"
}

install_tool kind "${KIND_VERSION}" "${KIND_SHA256}" \
  "https://github.com/kubernetes-sigs/kind/releases/download/${KIND_VERSION}/kind-linux-amd64"
install_tool kubectl "${KUBECTL_VERSION}" "${KUBECTL_SHA256}" \
  "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl"

command -v helm >/dev/null || die "helm not found in PATH"
command -v envsubst >/dev/null || die "envsubst not found in PATH (package gettext-base)"
