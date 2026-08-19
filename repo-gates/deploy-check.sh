#!/usr/bin/env bash
#
# deploy-check.sh - prove the deploy artifacts are valid BEFORE we ship, so a
# production deploy never blows up on a typo. Covers both deploy styles:
#
#   KAMAL  (pinned to 2.11.0)
#     - the kamal version in use is exactly 2.11.0
#     - .kamal/version and Gemfile agree on 2.11.0
#     - config/deploy.yml parses and has the required keys
#     - `kamal config` renders (best effort; needs secrets/registry)
#
#   RUBY DEPLOY TOOLCHAIN
#     - .ruby-version exists and pins 3.3.6, and CI installs that same Ruby
#     - Gemfile.lock exists, is TRACKED IN GIT, and resolves kamal 2.11.0
#     - Gemfile and Gemfile.lock have not drifted apart
#     - `bundle exec kamal` (the locked runner) is the one reporting 2.11.0
#
#   HELM
#     - `helm lint deploy/helm/lago`
#     - `helm template deploy/helm/lago` renders without error
#
# Tools are found in this order: native binary -> bundler (kamal) -> pinned
# docker image. If none is available the check SKIPs (and FAILs under STRICT,
# e.g. in CI where these MUST run).
#
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "${HERE}/lib.sh"

ROOT="$(repo_root)"
cd "${ROOT}"

# A gate must never mutate the artifact it judges. Bundler will silently
# re-resolve and REWRITE Gemfile.lock when it is missing or inconsistent with
# the Gemfile — which is exactly the state this gate exists to catch. Frozen
# mode makes bundler refuse to write, so a broken lockfile stays broken and is
# reported instead of self-healing behind our back. (Verified: without this,
# deleting Gemfile.lock or drifting it to kamal 1.9.0 left the gate GREEN.)
export BUNDLE_FROZEN=true

KAMAL_VERSION_REQUIRED="2.11.0"
KAMAL_IMAGE="ghcr.io/basecamp/kamal:${KAMAL_VERSION_REQUIRED}"
HELM_IMAGE="alpine/helm:3.16.2"   # pinned, never :latest
HELM_CHART="deploy/helm/lago"

############################  RUBY DEPLOY TOOLCHAIN  ##########################
# The Gemfile pin above only fixes the *requested* version. What actually runs
# under `bundle exec kamal` is whatever Gemfile.lock resolved — so an untracked,
# missing, or drifted lockfile means the deploy toolchain is not reproducible
# and a fresh `bundle install` could land a different Kamal major (the 1->2
# schema break we already paid for once). These checks make that state RED
# instead of invisible.
section "Ruby deploy toolchain (lockfile + .ruby-version)"

RUBY_VERSION_REQUIRED="3.3.6"

# --- .ruby-version: pin the interpreter, not just the gem --------------------
if [[ -f .ruby-version ]]; then
  rv="$(tr -d '[:space:]' < .ruby-version)"
  if [[ "${rv}" == "${RUBY_VERSION_REQUIRED}" ]]; then
    pass ".ruby-version pins ${RUBY_VERSION_REQUIRED}"
  else
    fail ".ruby-version is '${rv}', expected ${RUBY_VERSION_REQUIRED}"
  fi
else
  fail ".ruby-version missing (estate convention: pin the Ruby, not just the gem)"
fi

# CI must install the SAME Ruby the repo pins, or the lockfile it resolves
# against is not the one operators get.
CI_WORKFLOW=".github/workflows/hardening-gates.yml"
if [[ -f "${CI_WORKFLOW}" ]]; then
  ci_ruby="$(grep -oE '^[[:space:]]*RUBY_VERSION:[[:space:]]*"?[0-9.]+' "${CI_WORKFLOW}" \
             | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -n1)"
  if [[ -z "${ci_ruby}" ]]; then
    fail "${CI_WORKFLOW} declares no RUBY_VERSION"
  elif [[ "${ci_ruby}" == "${RUBY_VERSION_REQUIRED}" ]]; then
    pass "${CI_WORKFLOW} RUBY_VERSION agrees with .ruby-version"
  else
    fail "${CI_WORKFLOW} RUBY_VERSION is ${ci_ruby}, .ruby-version pins ${RUBY_VERSION_REQUIRED}"
  fi
else
  skip "${CI_WORKFLOW} not found; cannot cross-check CI Ruby"
fi

# --- Gemfile.lock: present, tracked, and pinning the right Kamal ------------
if [[ ! -f Gemfile.lock ]]; then
  fail "Gemfile.lock missing - run 'bundle install' and COMMIT the lockfile"
else
  pass "Gemfile.lock present"

  # Untracked is the failure mode this check exists for: the lockfile can sit
  # on one operator's disk and never reach the repo, so CI and every other
  # operator silently re-resolve.
  if have git && git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    if git ls-files --error-unmatch Gemfile.lock >/dev/null 2>&1; then
      pass "Gemfile.lock is tracked in git"
    else
      fail "Gemfile.lock is NOT tracked in git - 'git add Gemfile.lock' (an untracked lockfile is not a lockfile)"
    fi
  else
    skip "git not available; cannot verify Gemfile.lock is tracked"
  fi

  # The resolved kamal must be the pinned one, in BOTH places the lock states
  # it: the resolved spec and the DEPENDENCIES requirement.
  if grep -qE "^[[:space:]]+kamal \(${KAMAL_VERSION_REQUIRED//./\\.}\)" Gemfile.lock; then
    pass "Gemfile.lock resolves kamal to ${KAMAL_VERSION_REQUIRED}"
  else
    locked="$(grep -oE '^[[:space:]]+kamal \([0-9][^)]*\)' Gemfile.lock | head -n1 | tr -d ' ')"
    fail "Gemfile.lock resolves kamal to '${locked:-<none>}', expected ${KAMAL_VERSION_REQUIRED}"
  fi
  if grep -qE "^[[:space:]]+kamal \(= ${KAMAL_VERSION_REQUIRED//./\\.}\)" Gemfile.lock; then
    pass "Gemfile.lock DEPENDENCIES requires kamal (= ${KAMAL_VERSION_REQUIRED})"
  else
    fail "Gemfile.lock DEPENDENCIES does not require kamal (= ${KAMAL_VERSION_REQUIRED})"
  fi

  # Drift: every gem the Gemfile declares must appear in the lock's
  # DEPENDENCIES at the same exact version, and the lock must declare no
  # extras. This is a pure text comparison so it holds with no gems installed.
  gemfile_deps="$(grep -oE '^[[:space:]]*gem[[:space:]]+"[^"]+"[[:space:]]*,[[:space:]]*"[^"]+"' Gemfile \
                  | sed -E 's/.*gem[[:space:]]+"([^"]+)"[[:space:]]*,[[:space:]]*"([^"]+)".*/\1 (= \2)/' | sort)"
  lock_deps="$(awk '/^DEPENDENCIES$/{f=1;next} /^[A-Z]/{f=0} f && NF' Gemfile.lock \
               | sed -E 's/^[[:space:]]+//' | sort)"
  if [[ -z "${gemfile_deps}" ]]; then
    fail "no exactly-pinned gems parsed from Gemfile (expected at least kamal)"
  elif [[ "${gemfile_deps}" == "${lock_deps}" ]]; then
    pass "Gemfile and Gemfile.lock DEPENDENCIES agree (no drift)"
  else
    fail "Gemfile.lock has drifted from Gemfile"
    note "Gemfile:      $(echo "${gemfile_deps}" | tr '\n' ';')"
    note "Gemfile.lock: $(echo "${lock_deps}" | tr '\n' ';')"
  fi
fi

# --- the runner really is the locked one ------------------------------------
# `bundle exec` is what makes the lock binding; a bare `kamal` on PATH could be
# any major. Assert the bundled runner is the one that answered above.
if have bundle && bundle exec kamal version >/dev/null 2>&1; then
  bver="$(bundle exec kamal version 2>/dev/null | tr -d '[:space:]')"
  if [[ "${bver}" == "${KAMAL_VERSION_REQUIRED}" ]]; then
    pass "bundle exec kamal reports ${bver} (locked toolchain is what runs)"
  else
    fail "bundle exec kamal reports ${bver}, expected ${KAMAL_VERSION_REQUIRED}"
  fi
else
  skip "bundler cannot run kamal here (run 'bundle install'); lockfile checked statically above"
fi

############################  KAMAL  ##########################################
section "Kamal (required version ${KAMAL_VERSION_REQUIRED})"

# Resolve how we'll run kamal. Echoes a runner prefix or nothing.
KAMAL_RUNNER=""
if have bundle && bundle exec kamal version >/dev/null 2>&1; then
  KAMAL_RUNNER="bundle exec kamal"
elif have kamal; then
  KAMAL_RUNNER="kamal"
elif have docker; then
  if docker image inspect "${KAMAL_IMAGE}" >/dev/null 2>&1; then
    KAMAL_RUNNER="docker run --rm -v ${ROOT}:/workdir -w /workdir ${KAMAL_IMAGE}"
  fi
fi

# .kamal/version pin marker
if [[ -f .kamal/version ]]; then
  v="$(tr -d '[:space:]' < .kamal/version)"
  if [[ "${v}" == "${KAMAL_VERSION_REQUIRED}" ]]; then
    pass ".kamal/version pins ${KAMAL_VERSION_REQUIRED}"
  else
    fail ".kamal/version is '${v}', expected ${KAMAL_VERSION_REQUIRED}"
  fi
else
  fail ".kamal/version marker missing"
fi

# Gemfile pin
if [[ -f Gemfile ]]; then
  if grep -qE "kamal['\"][[:space:]]*,[[:space:]]*['\"](=[[:space:]]*)?${KAMAL_VERSION_REQUIRED//./\\.}" Gemfile; then
    pass "Gemfile pins kamal ${KAMAL_VERSION_REQUIRED}"
  else
    fail "Gemfile does not pin kamal to ${KAMAL_VERSION_REQUIRED}"
  fi
else
  skip "no Gemfile to pin kamal"
fi

# Installed version actually equals 2.11.0
if [[ -n "${KAMAL_RUNNER}" ]]; then
  kver="$(${KAMAL_RUNNER} version 2>/dev/null | tr -d '[:space:]' || true)"
  if [[ "${kver}" == "${KAMAL_VERSION_REQUIRED}" ]]; then
    pass "kamal binary reports ${kver}"
  elif [[ -n "${kver}" ]]; then
    fail "kamal binary reports ${kver}, expected ${KAMAL_VERSION_REQUIRED}"
  else
    skip "could not read kamal version"
  fi
else
  skip "kamal not available (install gem 'kamal' 2.11.0, or pull ${KAMAL_IMAGE})"
fi

# config/deploy.yml structure
if [[ -f config/deploy.yml ]]; then
  if have ruby; then
    if ruby -ryaml -e 'YAML.load_file("config/deploy.yml")' >/dev/null 2>&1; then
      pass "config/deploy.yml is valid YAML"
    else
      fail "config/deploy.yml is not valid YAML"
    fi
  fi
  missing=()
  for key in service image servers registry; do
    grep -qE "^${key}:" config/deploy.yml || missing+=("${key}")
  done
  if (( ${#missing[@]} == 0 )); then
    pass "config/deploy.yml has required keys (service, image, servers, registry)"
  else
    fail "config/deploy.yml missing keys: ${missing[*]}"
  fi

  # Render check. `kamal config` resolves env.secret names from .kamal/secrets;
  # supply a throwaway one (placeholders from the example) so it can fully render
  # without real secrets. This is removed immediately and never committed.
  if [[ -n "${KAMAL_RUNNER}" ]]; then
    tmp_secrets=0
    if [[ ! -f .kamal/secrets && -f .kamal/secrets.example ]]; then
      sed -E 's/^([A-Za-z_][A-Za-z0-9_]*)=.*/\1=placeholder/' .kamal/secrets.example > .kamal/secrets
      tmp_secrets=1
    fi
    if ${KAMAL_RUNNER} config >.kamal.log 2>&1; then
      pass "kamal config renders"
    else
      if grep -qiE 'no such host|network|registry|connection refused' .kamal.log; then
        skip "kamal config needs registry/network to fully render"
      else
        fail "kamal config errored"
        note "$(tail -n 12 .kamal.log)"
      fi
    fi
    rm -f .kamal.log
    (( tmp_secrets == 1 )) && rm -f .kamal/secrets
  fi
else
  fail "config/deploy.yml missing"
fi

############################  HELM  ###########################################
section "Helm chart (${HELM_CHART})"

HELM_RUNNER=""
if have helm; then
  HELM_RUNNER="helm"
elif have docker && docker image inspect "${HELM_IMAGE}" >/dev/null 2>&1; then
  HELM_RUNNER="docker run --rm -v ${ROOT}:/apps -w /apps ${HELM_IMAGE}"
fi

if [[ ! -d "${HELM_CHART}" ]]; then
  fail "${HELM_CHART} not found"
elif [[ -z "${HELM_RUNNER}" ]]; then
  skip "helm not available (install helm, or pull ${HELM_IMAGE})"
else
  if ${HELM_RUNNER} lint "${HELM_CHART}" >.helm.log 2>&1; then
    pass "helm lint"
  else
    fail "helm lint"
    note "$(tail -n 15 .helm.log)"
  fi
  rm -f .helm.log

  if ${HELM_RUNNER} template lago "${HELM_CHART}" >.helm.log 2>&1; then
    pass "helm template renders"
  else
    fail "helm template"
    note "$(tail -n 15 .helm.log)"
  fi
  rm -f .helm.log
fi

finish "Deploy gate (Kamal + Helm)"
exit $?
